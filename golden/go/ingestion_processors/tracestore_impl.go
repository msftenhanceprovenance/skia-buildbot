package ingestion_processors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.skia.org/infra/go/eventbus"
	"go.skia.org/infra/go/ingestion"
	"go.skia.org/infra/go/sharedconfig"
	"go.skia.org/infra/go/skerr"
	"go.skia.org/infra/go/sklog"
	"go.skia.org/infra/go/vcsinfo"
	"go.skia.org/infra/golden/go/jsonio"
	"go.skia.org/infra/golden/go/shared"
	"go.skia.org/infra/golden/go/tracestore"
	"go.skia.org/infra/golden/go/tracestore/bt_tracestore"
	"go.skia.org/infra/golden/go/types"
)

const (
	// Configuration option that identifies a tracestore backed by BigTable.
	btGoldIngester = "gold-bt"

	btProjectConfig  = "BTProjectID"
	btInstanceConfig = "BTInstance"
	btTableConfig    = "BTTable"
)

// Register the processor with the ingestion framework.
func init() {
	ingestion.Register(btGoldIngester, newBTTraceStoreProcessor)
}

// newTraceStoreProcessor implements the ingestion.Constructor signature and creates
// a Processor that uses a BigTable-backed tracestore.
func newBTTraceStoreProcessor(vcs vcsinfo.VCS, config *sharedconfig.IngesterConfig, _ *http.Client, _ eventbus.EventBus) (ingestion.Processor, error) {
	btc := bt_tracestore.BTConfig{
		ProjectID:  config.ExtraParams[btProjectConfig],
		InstanceID: config.ExtraParams[btInstanceConfig],
		TableID:    config.ExtraParams[btTableConfig],
		VCS:        vcs,
	}

	bts, err := bt_tracestore.New(context.TODO(), btc, true)
	if err != nil {
		return nil, skerr.Fmt("could not instantiate BT tracestore: %s", err)
	}
	return &btProcessor{
		ts:  bts,
		vcs: btc.VCS,
	}, nil
}

// btProcessor implements the ingestion.Processor interface for gold using
// the BigTable TraceStore
type btProcessor struct {
	ts  tracestore.TraceStore
	vcs vcsinfo.VCS
}

// Process implements the ingestion.Processor interface.
func (b *btProcessor) Process(ctx context.Context, resultsFile ingestion.ResultFileLocation) error {
	dmResults, err := processDMResults(resultsFile)
	if err != nil {
		return skerr.Fmt("could not process results file: %s", err)
	}

	if len(dmResults.Results) == 0 {
		sklog.Infof("ignoring file %s because it has no results", resultsFile.Name())
		return ingestion.IgnoreResultsFileErr
	}

	var commit *vcsinfo.LongCommit = nil
	// If the target commit is not in the primary repository we look it up
	// in the secondary that has the primary as a dependency.
	targetHash, err := getCanonicalCommitHash(ctx, b.vcs, dmResults.GitHash)
	if err != nil {
		if err == ingestion.IgnoreResultsFileErr {
			return ingestion.IgnoreResultsFileErr
		}
		return skerr.Fmt("could not identify canonical commit from %q: %s", dmResults.GitHash, err)
	}

	commit, err = b.vcs.Details(ctx, targetHash, true)
	if err != nil {
		return skerr.Fmt("could not get details for git commit %q: %s", targetHash, err)
	}

	if !commit.Branches["master"] {
		sklog.Warningf("Commit %s is not in master branch. Got branches: %v", commit.Hash, commit.Branches)
		return ingestion.IgnoreResultsFileErr
	}

	// Get the entries that should be added to the tracestore.
	entries, err := extractTraceStoreEntries(dmResults)
	if err != nil {
		return skerr.Fmt("could not create entries for results: %s", err)
	}

	t := time.Unix(resultsFile.TimeStamp(), 0)

	defer shared.NewMetricsTimer("put_tracestore_entry").Stop()

	sklog.Debugf("tracestore.Put(%s, %d entries, %s)", targetHash, len(entries), t)
	// Write the result to the tracestore.
	err = b.ts.Put(ctx, targetHash, entries, t)
	if err != nil {
		return skerr.Fmt("could not add to tracedb: %s", err)
	}
	return nil
}

// BatchFinished implements the ingestion.Processor interface.
func (b *btProcessor) BatchFinished() error { return nil }

// extractTraceStoreEntries creates a slice of tracestore.Entry for the given
// file. It will omit any entries that should be ignored. It returns an
// error if there were no un-ignored entries in the file.
func extractTraceStoreEntries(dm *dmResults) ([]*tracestore.Entry, error) {
	ret := make([]*tracestore.Entry, 0, len(dm.Results))
	for _, result := range dm.Results {
		params, options := paramsAndOptions(dm, result)
		if err := shouldIngest(params, options); err != nil {
			sklog.Infof("Not ingesting %s : %s", dm.name, err)
			continue
		}

		ret = append(ret, &tracestore.Entry{
			Params:  params,
			Options: options,
			Digest:  result.Digest,
		})
	}

	// If all results were ignored then we return an error.
	if len(ret) == 0 {
		return nil, fmt.Errorf("No valid results in file %s.", dm.name)
	}

	return ret, nil
}

// paramsAndOptions creates the params and options maps from a given file and entry.
func paramsAndOptions(dm *dmResults, r *jsonio.Result) (map[string]string, map[string]string) {
	params := make(map[string]string, len(dm.Key)+len(r.Key))
	for k, v := range dm.Key {
		params[k] = v
	}
	for k, v := range r.Key {
		params[k] = v
	}
	return params, r.Options
}

// shouldIngest returns a descriptive error if we should ignore an entry
// with these params/options.
func shouldIngest(params, options map[string]string) error {
	// Ignore anything that is not a png. In the early days (pre-2015), ext was omitted
	// but implied to be "png". Thus if ext is not provided, it will be ingested.
	// New entries (created by goldctl) will always have ext set.
	if ext, ok := options["ext"]; ok && (ext != "png") {
		return errors.New("ignoring non-png entry")
	}

	// Make sure the test name meets basic requirements.
	testName := params[types.PRIMARY_KEY_FIELD]

	// Ignore results that don't have a test given and log an error since that
	// should not happen. But we want to keep other results in the same input file.
	if testName == "" {
		return errors.New("missing test name")
	}

	// Make sure the test name does not exceed the allowed length.
	if len(testName) > types.MAXIMUM_NAME_LENGTH {
		return fmt.Errorf("Received test name which is longer than the allowed %d bytes: %s", types.MAXIMUM_NAME_LENGTH, testName)
	}

	return nil
}
