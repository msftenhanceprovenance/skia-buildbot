package aggregator

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	go_metrics "github.com/rcrowley/go-metrics"
	"github.com/skia-dev/glog"
	"go.skia.org/infra/fuzzer/go/common"
	"go.skia.org/infra/fuzzer/go/config"
	"go.skia.org/infra/fuzzer/go/fuzz"
	"go.skia.org/infra/go/exec"
	"go.skia.org/infra/go/fileutil"
	"go.skia.org/infra/go/util"
	"golang.org/x/net/context"
	"google.golang.org/cloud/storage"
)

// BinaryAggregator is a key part of the fuzzing operation
// (see https://skia.googlesource.com/buildbot/+/master/fuzzer/DESIGN.md).
// It will find new bad binary fuzzes generated by afl-fuzz and create the
// metadata required for them.
// It does this by searching in the specified AflOutputPath for new crashes and moves them to a
// temporary holding folder (specified by BinaryFuzzPath) for parsing, before sending
// them through the "analysis pipeline".  This pipeline has three steps, Analysis, Upload and
// Bug Reporting.  Analysis runs the fuzz against a debug and release version of Skia
// which produces stacktraces and error output.  Upload uploads these pieces to
// Google Storage (GCS).  Bug Reporting is used to either create or update a bug related to
// the given fuzz.
type BinaryAggregator struct {
	// Should be set to true if a bug should be created for every bad fuzz found.
	// Example: Detecting regressions.
	// TODO(kjlubick): consider making this a function that clients supply so they can decide.
	MakeBugOnBadFuzz bool
	// Should be set if we want to upload grey fuzzes.  This should only be true
	// if we are changing versions.
	UploadGreyFuzzes bool

	storageClient *storage.Client
	// For passing the paths of new binaries that should be scanned.
	forAnalysis chan string
	// For passing the file names of analyzed fuzzes that should be uploaded from where they rest on
	// disk in config.Aggregator.BinaryFuzzPath
	forUpload chan uploadPackage

	forBugReporting chan bugReportingPackage
	// The shutdown channels are used to signal shutdowns.  There are two groups, to
	// allow for a softer, cleaner shutdown w/ minimal lost work.
	// Group A includes the scanning and the monitoring routine.
	// Group B include the analysis and upload routines and the bug reporting routine.
	monitoringShutdown  chan bool
	monitoringWaitGroup *sync.WaitGroup
	pipelineShutdown    chan bool
	pipelineWaitGroup   *sync.WaitGroup
	// These three counts are used to determine if there is any pending work.
	// There is no pending work if all three of these values are equal and the
	// work queues are empty.
	analysisCount  int64
	uploadCount    int64
	bugReportCount int64
}

// AnalysisPackage is a generic holder for the functions needed to analyze
type AnalysisPackage struct {
	Setup   func(workingDirPath string) error
	Analyze func(workingDirPath, pathToFile string) (uploadPackage, error)
}

const (
	BAD_FUZZ  = "bad"
	GREY_FUZZ = "grey"
)

// uploadPackage is a struct containing all the pieces of a fuzz that need to be uploaded to GCS
type uploadPackage struct {
	Name        string
	FilePath    string
	DebugDump   string
	DebugErr    string
	ReleaseDump string
	ReleaseErr  string
	FileType    string
	// Must be BAD_FUZZ or GREY_FUZZ
	FuzzType string
}

// bugReportingPackage is a struct containing the pieces of a fuzz that may need to have
// a bug filed or updated.
type bugReportingPackage struct {
	FuzzName   string
	CommitHash string
	IsBadFuzz  bool
}

// StartBinaryAggregator creates and starts a BinaryAggregator.
// If there is a problem starting up, an error is returned.  Other errors will be logged.
func StartBinaryAggregator(s *storage.Client) (*BinaryAggregator, error) {
	b := BinaryAggregator{
		storageClient:      s,
		forAnalysis:        make(chan string, 10000),
		forUpload:          make(chan uploadPackage, 100),
		forBugReporting:    make(chan bugReportingPackage, 100),
		MakeBugOnBadFuzz:   false,
		monitoringShutdown: make(chan bool, 2),
		// pipelineShutdown needs to be created with a calculated capacity in start
	}

	return &b, b.start()
}

// start starts up the BinaryAggregator.  It refreshes all status it needs and builds
// a debug and a release version of Skia for use in analysis.  It then spawns the
// aggregation pipeline and a monitoring thread.
func (agg *BinaryAggregator) start() error {
	// Set the wait groups to fresh
	agg.monitoringWaitGroup = &sync.WaitGroup{}
	agg.pipelineWaitGroup = &sync.WaitGroup{}
	agg.analysisCount = 0
	agg.uploadCount = 0
	agg.bugReportCount = 0
	if _, err := fileutil.EnsureDirExists(config.Aggregator.BinaryFuzzPath); err != nil {
		return err
	}
	if _, err := fileutil.EnsureDirExists(config.Aggregator.ExecutablePath); err != nil {
		return err
	}
	if err := common.BuildClangHarness("Debug", true); err != nil {
		return err
	}
	if err := common.BuildClangHarness("Release", true); err != nil {
		return err
	}

	agg.monitoringWaitGroup.Add(1)
	go agg.scanForNewCandidates()

	numAnalysisProcesses := config.Aggregator.NumAnalysisProcesses
	if numAnalysisProcesses <= 0 {
		// TODO(kjlubick): Actually make this smart based on the number of cores
		numAnalysisProcesses = 20
	}
	for i := 0; i < numAnalysisProcesses; i++ {
		agg.pipelineWaitGroup.Add(1)
		go agg.waitForAnalysis(i, analyzeSkp)
	}

	numUploadProcesses := config.Aggregator.NumUploadProcesses
	if numUploadProcesses <= 0 {
		// TODO(kjlubick): Actually make this smart based on the number of cores/number
		// of aggregation processes
		numUploadProcesses = 5
	}
	for i := 0; i < numUploadProcesses; i++ {
		agg.pipelineWaitGroup.Add(1)
		go agg.waitForUploads(i)
	}
	agg.pipelineWaitGroup.Add(1)
	go agg.waitForBugReporting()
	agg.pipelineShutdown = make(chan bool, numAnalysisProcesses+numUploadProcesses+1)
	// start background routine to monitor queue details
	agg.monitoringWaitGroup.Add(1)
	go agg.monitorStatus(numAnalysisProcesses, numUploadProcesses)
	return nil
}

// scanForNewCandidates runs scanHelper once every config.Aggregator.RescanPeriod, which scans the
// config.Generator.AflOutputPath for new fuzzes.
// If scanHelper returns an error, this method will terminate.
func (agg *BinaryAggregator) scanForNewCandidates() {
	defer agg.monitoringWaitGroup.Done()

	alreadyFoundBinaries := &SortedStringSlice{}
	// time.Tick does not fire immediately, so we fire it manually once.
	if err := agg.scanHelper(alreadyFoundBinaries); err != nil {
		glog.Errorf("Scanner terminated due to error: %v", err)
		return
	}
	glog.Infof("Sleeping for %s, then waking up to find new crashes again", config.Aggregator.RescanPeriod)

	t := time.Tick(config.Aggregator.RescanPeriod)
	for {
		select {
		case <-t:
			if err := agg.scanHelper(alreadyFoundBinaries); err != nil {
				glog.Errorf("Aggregator scanner terminated due to error: %v", err)
				return
			}
			glog.Infof("Sleeping for %s, then waking up to find new crashes again", config.Aggregator.RescanPeriod)
		case <-agg.monitoringShutdown:
			glog.Info("Aggregator scanner got signal to shut down")
			return
		}

	}
}

// scanHelper runs findBadBinaryPaths, logs the output and keeps alreadyFoundBinaries up to date.
func (agg *BinaryAggregator) scanHelper(alreadyFoundBinaries *SortedStringSlice) error {
	newlyFound, err := findBadBinaryPaths(alreadyFoundBinaries)
	if err != nil {
		return err
	}
	// AFL-fuzz does not write crashes or hangs atomically, so this workaround waits for a bit after
	// we have references to where the crashes will be.
	// TODO(kjlubick), switch to using flock once afl-fuzz implements that upstream.
	time.Sleep(time.Second)
	go_metrics.GetOrRegisterGauge("binary_newly_found_fuzzes", go_metrics.DefaultRegistry).Update(int64(len(newlyFound)))
	glog.Infof("%d newly found bad binary fuzzes", len(newlyFound))
	for _, f := range newlyFound {
		agg.forAnalysis <- f
	}
	alreadyFoundBinaries.Append(newlyFound)
	return nil
}

// findBadBinaryPaths looks through all the afl-fuzz directories contained in the passed in path and
// returns the path to all files that are in a crash* folder that are not already in
// 'alreadyFoundBinaries'
// It also sends them to the forAnalysis channel when it finds them.
// The output from afl-fuzz looks like:
// $AFL_ROOT/
//		-fuzzer0/
//			-crashes/  <-- bad binary fuzzes end up here
//			-hangs/
//			-queue/
//			-fuzzer_stats
//		-fuzzer1/
//		...
func findBadBinaryPaths(alreadyFoundBinaries *SortedStringSlice) ([]string, error) {
	badBinaryPaths := make([]string, 0)

	aflDir, err := os.Open(config.Generator.AflOutputPath)
	if err != nil {
		return nil, err
	}
	defer util.Close(aflDir)

	fuzzerFolders, err := aflDir.Readdir(-1)
	if err != nil {
		return nil, err
	}

	for _, fuzzerFolderInfo := range fuzzerFolders {
		// fuzzerFolderName an os.FileInfo like fuzzer0, fuzzer1
		path := filepath.Join(config.Generator.AflOutputPath, fuzzerFolderInfo.Name())
		fuzzerDir, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer util.Close(fuzzerDir)

		fuzzerContents, err := fuzzerDir.Readdir(-1)
		if err != nil {
			return nil, err
		}
		for _, info := range fuzzerContents {
			// Look through fuzzerN/crashes
			if info.IsDir() && strings.HasPrefix(info.Name(), "crashes") {
				crashPath := filepath.Join(path, info.Name())
				crashDir, err := os.Open(crashPath)
				if err != nil {
					return nil, err
				}
				defer util.Close(crashDir)

				crashContents, err := crashDir.Readdir(-1)
				if err != nil {
					return nil, err
				}
				for _, crash := range crashContents {
					// Make sure the files are actually crashable files we haven't found before
					if crash.Name() != "README.txt" {
						if fuzzPath := filepath.Join(crashPath, crash.Name()); !alreadyFoundBinaries.Contains(fuzzPath) {
							badBinaryPaths = append(badBinaryPaths, fuzzPath)
						}
					}
				}
			}
		}
	}
	return badBinaryPaths, nil
}

// waitForAnalysis waits for files that need to be analyzed (from forAnalysis) and makes a copy of
// them in config.Aggregator.BinaryFuzzPath with their hash as a file name.
// It then analyzes it using the supplied AnalysisPackage and then signals the results should be
// uploaded. If any unrecoverable errors happen, this method terminates.
func (agg *BinaryAggregator) waitForAnalysis(identifier int, analysisPackage AnalysisPackage) {
	defer agg.pipelineWaitGroup.Done()
	defer go_metrics.GetOrRegisterCounter("analysis_process_count", go_metrics.DefaultRegistry).Dec(int64(1))
	glog.Infof("Spawning analyzer %d", identifier)

	// our own unique working folder
	executableDir := filepath.Join(config.Aggregator.ExecutablePath, fmt.Sprintf("analyzer%d", identifier))
	if err := analysisPackage.Setup(executableDir); err != nil {
		glog.Errorf("Analyzer %d terminated due to error: %s", identifier, err)
		return
	}
	for {
		select {
		case badBinaryPath := <-agg.forAnalysis:
			atomic.AddInt64(&agg.analysisCount, int64(1))
			err := agg.analysisHelper(executableDir, badBinaryPath, analysisPackage)
			if err != nil {
				glog.Errorf("Analyzer %d terminated due to error: %s", identifier, err)
				return
			}
		case <-agg.pipelineShutdown:
			glog.Infof("Analyzer %d recieved shutdown signal", identifier)
			return
		}
	}
}

// analysisHelper performs the analysis on the given binary and returns an error
// if anything goes wrong.  On success, the results will be placed in the upload queue.
func (agg *BinaryAggregator) analysisHelper(executableDir, badBinaryPath string, analysisPackage AnalysisPackage) error {
	hash, data, err := calculateHash(badBinaryPath)
	if err != nil {
		return err
	}
	newFuzzPath := filepath.Join(config.Aggregator.BinaryFuzzPath, hash)
	if err := ioutil.WriteFile(newFuzzPath, data, 0644); err != nil {
		return err
	}
	if upload, err := analysisPackage.Analyze(executableDir, hash); err != nil {
		return fmt.Errorf("Problem analyzing %s, terminating: %s", newFuzzPath, err)
	} else {
		agg.forUpload <- upload
	}
	return nil
}

// analyzeSkp is an analysisPackage for analyzing skp files.
// Setup cleans out the work space, makes a copy of the Debug and Release parseskp executable.
// Analyze simply invokes performBinaryAnalysis using parse_skp on the files that are passed in.
var analyzeSkp = AnalysisPackage{
	Setup: func(workingDirPath string) error {
		// Delete all previous binaries to get a clean start
		if err := os.RemoveAll(workingDirPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(workingDirPath, 0755); err != nil {
			return err
		}

		// make a copy of the debug and release executables
		basePath := filepath.Join(config.Generator.SkiaRoot, "out")
		if err := fileutil.CopyExecutable(filepath.Join(basePath, "Debug", common.TEST_HARNESS_NAME), filepath.Join(workingDirPath, common.TEST_HARNESS_NAME+"_debug")); err != nil {
			return err
		}
		if err := fileutil.CopyExecutable(filepath.Join(basePath, "Release", common.TEST_HARNESS_NAME), filepath.Join(workingDirPath, common.TEST_HARNESS_NAME+"_release")); err != nil {
			return err
		}

		return nil
	},
	Analyze: func(workingDirPath, skpFileName string) (uploadPackage, error) {
		upload := uploadPackage{
			Name:     skpFileName,
			FileType: "skpicture",
			FuzzType: BAD_FUZZ,
			FilePath: filepath.Join(config.Aggregator.BinaryFuzzPath, skpFileName),
		}

		if dump, stderr, err := performBinaryAnalysis(workingDirPath, common.TEST_HARNESS_NAME, skpFileName, true); err != nil {
			return upload, err
		} else {
			upload.DebugDump = dump
			upload.DebugErr = stderr
		}
		if dump, stderr, err := performBinaryAnalysis(workingDirPath, common.TEST_HARNESS_NAME, skpFileName, false); err != nil {
			return upload, err
		} else {
			upload.ReleaseDump = dump
			upload.ReleaseErr = stderr
		}
		if r := fuzz.ParseFuzzResult(upload.DebugDump, upload.DebugErr, upload.ReleaseDump, upload.ReleaseErr); r.Flags == fuzz.DebugFailedGracefully|fuzz.ReleaseFailedGracefully {
			upload.FuzzType = GREY_FUZZ
		}
		return upload, nil
	},
}

// performBinaryAnalysis executes a command like:
// timeout AnalysisTimeout catchsegv ./parse_foo_debug --input badbeef
// from the working dir specified.
// GNU timeout is used instead of the option on exec.Command because experimentation with the latter
// showed evidence of that way leaking processes, which lead to OOM errors.
// GNU catchsegv generates human readable dumps of crashes, which can then be scanned for stacktrace
// information. The dumps (which come via standard out) and standard errors are recorded as strings.
func performBinaryAnalysis(workingDirPath, baseExecutableName, fileName string, isDebug bool) (string, string, error) {
	suffix := "_release"
	if isDebug {
		suffix = "_debug"
	}

	pathToFile := filepath.Join(config.Aggregator.BinaryFuzzPath, fileName)
	pathToExecutable := fmt.Sprintf("./%s%s", baseExecutableName, suffix)
	timeoutInSeconds := fmt.Sprintf("%ds", config.Aggregator.AnalysisTimeout/time.Second)

	var dump bytes.Buffer
	var stdErr bytes.Buffer

	cmd := &exec.Command{
		Name:      "timeout",
		Args:      []string{timeoutInSeconds, "catchsegv", pathToExecutable, "--type", "skp", "--bytes", pathToFile},
		LogStdout: false,
		LogStderr: false,
		Stdout:    &dump,
		Stderr:    &stdErr,
		Dir:       workingDirPath,
	}

	//errors are fine/expected from this, as we are dealing with bad fuzzes
	if err := exec.Run(cmd); err != nil {
		return dump.String(), stdErr.String(), nil
	}
	return dump.String(), stdErr.String(), nil
}

// calcuateHash calculates the sha1 hash of a file, given its path.  It returns both the hash as a
// hex-encoded string and the contents of the file.
func calculateHash(path string) (hash string, data []byte, err error) {
	data, err = ioutil.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("Problem reading file for hashing %s: %s", path, err)
	}
	return fmt.Sprintf("%x", sha1.Sum(data)), data, nil
}

// A SortedStringSlice has a sortable string slice which is always kept sorted.
// This allows for an implementation of Contains that runs in O(log n)
type SortedStringSlice struct {
	strings sort.StringSlice
}

// Contains returns true if the passed in string is in the underlying slice
func (s *SortedStringSlice) Contains(str string) bool {
	i := s.strings.Search(str)
	if i < len(s.strings) && s.strings[i] == str {
		return true
	}
	return false
}

// Append adds all of the strings to the underlying slice and sorts it
func (s *SortedStringSlice) Append(strs []string) {
	s.strings = append(s.strings, strs...)
	s.strings.Sort()
}

// waitForUploads waits for uploadPackages to be sent through the forUpload channel
// and then uploads them.  If any unrecoverable errors happen, this method terminates.
func (agg *BinaryAggregator) waitForUploads(identifier int) {
	defer agg.pipelineWaitGroup.Done()
	defer go_metrics.GetOrRegisterCounter("upload_process_count", go_metrics.DefaultRegistry).Dec(int64(1))
	glog.Infof("Spawning uploader %d", identifier)
	for {
		select {
		case p := <-agg.forUpload:
			atomic.AddInt64(&agg.uploadCount, int64(1))
			if !agg.UploadGreyFuzzes && p.FuzzType == GREY_FUZZ {
				glog.Infof("Skipping upload of grey fuzz %s", p.Name)
				continue
			}
			if err := agg.upload(p); err != nil {
				glog.Errorf("Uploader %d terminated due to error: %s", identifier, err)
				return
			}
			agg.forBugReporting <- bugReportingPackage{
				FuzzName:   p.Name,
				CommitHash: config.Generator.SkiaVersion.Hash,
				IsBadFuzz:  p.FuzzType == BAD_FUZZ,
			}
		case <-agg.pipelineShutdown:
			glog.Infof("Uploader %d recieved shutdown signal", identifier)
			return
		}
	}
}

// upload breaks apart the uploadPackage into its constituant parts and uploads them to
// GCS using some helper methods.
func (agg *BinaryAggregator) upload(p uploadPackage) error {
	glog.Infof("uploading %s with file %s and analysis bytes %d;%d;%d;%d ", p.Name, p.FilePath, len(p.DebugDump), len(p.DebugErr), len(p.ReleaseDump), len(p.ReleaseErr))

	if err := agg.uploadBinaryFromDisk(p, p.Name, p.FilePath); err != nil {
		return err
	}
	if err := agg.uploadString(p, p.Name+"_debug.dump", p.DebugDump); err != nil {
		return err
	}
	if err := agg.uploadString(p, p.Name+"_debug.err", p.DebugErr); err != nil {
		return err
	}
	if err := agg.uploadString(p, p.Name+"_release.dump", p.ReleaseDump); err != nil {
		return err
	}
	return agg.uploadString(p, p.Name+"_release.err", p.ReleaseErr)
}

// uploadBinaryFromDisk uploads a binary file on disk to GCS, returning an error if
// anything goes wrong.
func (agg *BinaryAggregator) uploadBinaryFromDisk(p uploadPackage, fileName, filePath string) error {
	name := fmt.Sprintf("%s/%s/%s/%s/%s", p.FileType, config.Generator.SkiaVersion.Hash, p.FuzzType, p.Name, fileName)
	w := agg.storageClient.Bucket(config.GS.Bucket).Object(name).NewWriter(context.Background())
	defer util.Close(w)
	// We set the encoding to avoid accidental crashes if Chrome were to try to render
	// a fuzzed png or svg or something.
	w.ObjectAttrs.ContentEncoding = "application/octet-stream"

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("There was a problem reading %s for uploading : %s", filePath, err)
	}

	if n, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("There was a problem uploading binary %s.  Only uploaded %d bytes : %s", name, n, err)
	}
	return nil
}

// uploadBinaryFromDisk uploads the contents of a string as a file to GCS, returning an error if
// anything goes wrong.
func (agg *BinaryAggregator) uploadString(p uploadPackage, fileName, contents string) error {
	name := fmt.Sprintf("%s/%s/%s/%s/%s", p.FileType, config.Generator.SkiaVersion.Hash, p.FuzzType, p.Name, fileName)
	w := agg.storageClient.Bucket(config.GS.Bucket).Object(name).NewWriter(context.Background())
	defer util.Close(w)
	w.ObjectAttrs.ContentEncoding = "text/plain"

	if n, err := w.Write([]byte(contents)); err != nil {
		return fmt.Errorf("There was a problem uploading %s.  Only uploaded %d bytes: %s", name, n, err)
	}
	return nil
}

// waitForUploads waits for uploadPackages to be sent through the forUpload channel
// and then uploads them.  If any unrecoverable errors happen, this method terminates.
func (agg *BinaryAggregator) waitForBugReporting() {
	defer agg.pipelineWaitGroup.Done()
	glog.Info("Spawning bug reporting routine")
	for {
		select {
		case p := <-agg.forBugReporting:
			if err := agg.bugReportingHelper(p); err != nil {
				glog.Errorf("Bug reporting terminated due to error: %s", err)
				return
			}
		case <-agg.pipelineShutdown:
			glog.Info("Bug reporting routine recieved shutdown signal")
			return
		}
	}
}

// bugReportingHelper is a helper function to report bugs if the aggregator is configured to.
func (agg *BinaryAggregator) bugReportingHelper(p bugReportingPackage) error {
	defer atomic.AddInt64(&agg.bugReportCount, int64(1))
	if agg.MakeBugOnBadFuzz && p.IsBadFuzz {
		glog.Warningf("Should create bug for %s", p.FuzzName)
	}
	return nil
}

// monitorStatus sets up the monitoring routine, which reports how big the work queues are
// and how many processes are up.
func (agg *BinaryAggregator) monitorStatus(numAnalysisProcesses, numUploadProcesses int) {
	defer agg.monitoringWaitGroup.Done()
	analysisProcessCount := go_metrics.GetOrRegisterCounter("analysis_process_count", go_metrics.DefaultRegistry)
	analysisProcessCount.Clear()
	analysisProcessCount.Inc(int64(numAnalysisProcesses))
	uploadProcessCount := go_metrics.GetOrRegisterCounter("upload_process_count", go_metrics.DefaultRegistry)
	uploadProcessCount.Clear()
	uploadProcessCount.Inc(int64(numUploadProcesses))

	t := time.Tick(config.Aggregator.StatusPeriod)
	for {
		select {
		case <-agg.monitoringShutdown:
			glog.Infof("aggregator monitor got signal to shut down")
			return
		case <-t:
			go_metrics.GetOrRegisterGauge("binary_analysis_queue_size", go_metrics.DefaultRegistry).Update(int64(len(agg.forAnalysis)))
			go_metrics.GetOrRegisterGauge("binary_upload_queue_size", go_metrics.DefaultRegistry).Update(int64(len(agg.forUpload)))
			go_metrics.GetOrRegisterGauge("binary_bug_report_queue_size", go_metrics.DefaultRegistry).Update(int64(len(agg.forBugReporting)))
		}
	}
}

// Shutdown gracefully shuts down the aggregator. Anything
// that was being processed will finish prior to the shutdown.
func (agg *BinaryAggregator) ShutDown() {
	// once for the monitoring and once for the scanning routines
	agg.monitoringShutdown <- true
	agg.monitoringShutdown <- true
	agg.monitoringWaitGroup.Wait()
	// wait for everything to finish analysis and upload
	agg.WaitForEmptyQueues()

	// signal once for every group b thread we started, which is the
	// capacity of our pipelineShutdown channel.
	for i := len(agg.pipelineShutdown); i < cap(agg.pipelineShutdown); i++ {
		agg.pipelineShutdown <- true
	}
	agg.pipelineWaitGroup.Wait()
}

// RestartAnalysis restarts the shut down aggregator.  Anything that is in the scanning
// directory should be cleared out, lest it be rescanned/analyzed.
func (agg *BinaryAggregator) RestartAnalysis() error {
	return agg.start()
}

// WaitForEmptyQueues will return once there is nothing more in the analysis-upload-report
// pipeline, waiting in increments of config.Aggregator.StatusPeriod until it is done.
func (agg *BinaryAggregator) WaitForEmptyQueues() {
	a := len(agg.forAnalysis)
	u := len(agg.forUpload)
	b := len(agg.forBugReporting)
	if a == 0 && u == 0 && b == 0 && agg.analysisCount == agg.uploadCount && agg.uploadCount == agg.bugReportCount {
		glog.Infof("Queues were already empty")
		return
	}
	t := time.Tick(config.Aggregator.StatusPeriod)
	glog.Infof("Waiting %s for the aggregator's queues to be empty", config.Aggregator.StatusPeriod)
	for _ = range t {
		a = len(agg.forAnalysis)
		u = len(agg.forUpload)
		b = len(agg.forBugReporting)
		glog.Infof("AnalysisQueue: %d, UploadQueue: %d, BugReportingQueue: %d", a, u, b)
		glog.Infof("AnalysisTotal: %d, UploadTotal: %d, BugReportingTotal: %d", agg.analysisCount, agg.uploadCount, agg.bugReportCount)
		if a == 0 && u == 0 && b == 0 && agg.analysisCount == agg.uploadCount && agg.uploadCount == agg.bugReportCount {
			break
		}
		glog.Infof("Waiting %s for the aggregator's queues to be empty", config.Aggregator.StatusPeriod)
	}
}

// ForceAnalysis directly adds the given path to the analysis queue, where it will be
// analyzed, uploaded and possibly bug reported.
func (agg *BinaryAggregator) ForceAnalysis(path string) {
	agg.forAnalysis <- path
}
