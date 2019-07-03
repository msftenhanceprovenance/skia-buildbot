package tracestore

import (
	"context"
	"regexp"
	"time"

	"go.skia.org/infra/go/paramtools"
	"go.skia.org/infra/go/query"
	"go.skia.org/infra/go/sklog"
	"go.skia.org/infra/go/tiling"
	"go.skia.org/infra/golden/go/types"
)

// Entry is one digests and related params to be added to the TraceStore.
type Entry struct {
	// Params describe the configuration that produced the digest/image.
	// These params will be used to form the trace id.
	Params map[string]string

	// Options give extra details about this trace. These options will be stored
	// tile by tile, with the most recent commit's options overwriting any
	// previous options. These details do not affect the trace id.
	Options map[string]string

	// Digest references the image that was generated by the test.
	Digest types.Digest
}

// TraceStore is the interface to store trace data.
type TraceStore interface {
	// Put writes the given entries to the TraceStore at the given commit hash. The timestamp is
	// assumed to be the time when the entries were generated.
	// It is undefined behavior to have multiple entries with the exact same Params.
	Put(ctx context.Context, commitHash string, entries []*Entry, ts time.Time) error

	// GetTile reads the last n commits and returns them as a tile.
	// The second return value is all commits that are in the tile.
	GetTile(ctx context.Context, nCommits int) (*tiling.Tile, []*tiling.Commit, error)

	// GetDenseTile constructs a tile containing only commits that have data for at least one trace.
	// The returned tile will always have length exactly nCommits unless there are fewer than
	// nCommits commits with data. The second return value contains all commits starting with the
	// first commit of the tile and ending with the most recent commit, in order; i.e. it includes
	// all commits in the tile as well as the omitted commits.
	GetDenseTile(ctx context.Context, nCommits int) (*tiling.Tile, []*tiling.Commit, error)
}

// TraceIDFromParams deterministically returns a TraceId that uniquely encodes
// the given params. It follows the same convention as perf's trace ids, that
// is something like ",key1=value1,key2=value2,...," where the keys
// are in alphabetical order.
func TraceIDFromParams(params paramtools.Params) tiling.TraceId {
	// Clean up any params with , or =
	params = forceValid(params)
	s, err := query.MakeKeyFast(params)
	if err != nil {
		sklog.Warningf("Invalid params passed in for trace id %#v: %s", params, err)
	}
	return tiling.TraceId(s)
}

var (
	invalidChar = regexp.MustCompile("([,=])")
)

func clean(s string) string {
	return invalidChar.ReplaceAllLiteralString(s, "_")
}

// forceValid ensures that the resulting map will make a valid structured key.
func forceValid(m map[string]string) map[string]string {
	ret := make(map[string]string, len(m))
	for key, value := range m {
		ret[clean(key)] = clean(value)
	}

	return ret
}
