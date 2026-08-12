package assembler

import (
	"testing"
	"time"
)

// waitForStableCacheUsage polls until the reported cache usage stops moving,
// which means the worker has drained the request channel. Returns the settled
// figure.
func waitForStableCacheUsage(t *testing.T, a *Assembler) int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last, stable := int64(-1), 0
	for time.Now().Before(deadline) {
		cur := a.CacheUsageBytes()
		if cur == last {
			if stable++; stable >= 3 {
				return cur
			}
		} else {
			last, stable = cur, 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cache usage never settled; last read %d", last)
	return 0
}

// TestWriteCachePressureIsRelievedOnTheWritePath pins B2's enforcement to the
// CALL SITE, not to relievePressure itself.
//
// This distinction is the whole point of the test. relievePressure already had
// two direct unit tests, and deleting `a.relievePressure(wc, open)` from
// processRequest left the entire suite green — the function was pinned and its
// wiring was not. That is not a bounded leak: writeCache.buffer has no limit
// check of any kind, it unconditionally caches and increments wc.used, so that
// one call is the sole enforcement of wc.limit anywhere in the process.
// Without it, cached article memory grows with the download rather than with
// the configured bound, which is exactly what B2 forbids.
//
// The fixture is built so nothing ELSE can drain the cache and mask the
// deletion:
//
//   - Articles total 40 KiB, well under contiguousRunSize (512 KiB), so
//     flushContiguous never fires and coalescing cannot do the draining.
//   - TotalParts is far higher than the number fed, so the file never
//     completes and finalizeFile's drain never runs.
//   - The assertion is taken while the assembler is still running, because
//     Stop drains everything and would read zero either way.
func TestWriteCachePressureIsRelievedOnTheWritePath(t *testing.T) {
	const (
		limit     = 8 << 10 // 8 KiB cache
		artSize   = 1 << 10 // 1 KiB per article
		numArts   = 40      // 40 KiB total, 5x the limit
		neverDone = 1000    // TotalParts the fixture will not reach
	)

	dir := t.TempDir()
	files := map[string]FileInfo{}
	registerFile(t, dir, files, "job1", 0, neverDone)
	opts := makeOpts(dir, files)
	opts.WriteCacheBytes = limit
	// One slot, so WriteArticle blocks until the worker takes the previous
	// request and the feed cannot outrun processing by an unbounded margin.
	opts.QueueSize = 1
	a := startAssembler(t, opts)

	for i := range numArts {
		if err := a.WriteArticle(t.Context(), WriteRequest{
			JobID: "job1", FileIdx: 0,
			ArtIdx:    int32(i), //nolint:gosec // G115: loop bound is 40
			MessageID: string(rune('a' + i%26)),
			Offset:    int64(i) * artSize,
			Data:      make([]byte, artSize),
		}); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}

	used := waitForStableCacheUsage(t, a)
	if used > limit {
		t.Errorf("write cache holds %d bytes with a %d-byte limit — B2 is not enforced. "+
			"writeCache.buffer has no limit check, so the relievePressure call in "+
			"processRequest is the only thing bounding this; deleting it makes cached "+
			"memory grow with the download instead of with the configuration", used, limit)
	}
	// Guard the fixture: if nothing was ever buffered, the bound above holds
	// vacuously and the test would pass with the call site deleted.
	if total := int64(numArts) * artSize; total <= limit {
		t.Fatalf("fixture feeds %d bytes into a %d-byte cache; it cannot exercise pressure", total, limit)
	}
}
