package queue

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// A large but realistic release: ~15 GB at 750 KB per article.
const (
	retentionFiles       = 100
	retentionPerFile     = 200
	retentionArticles    = retentionFiles * retentionPerFile
	retentionArticleSize = 750_000
	retentionSamples     = 8
)

// TestTerminalJobRetention_Measured is the evidence for declining terminal
// compaction (step 4 of docs/queue-lifecycle.md). It measures what a parked
// terminal job retains, and exists so the figures in that document can be
// re-derived rather than taken on trust.
//
// The two figures are measured by different means, deliberately.
//
// The compaction saving is computed exactly from the bitsets' backing arrays.
// Heap sampling cannot resolve it: at ~7 KB it sits below the allocator noise
// left by building a 1.4 MB manifest, and an earlier draft of this test
// reported a *negative* retained size for the compacted case and still passed,
// because the assertion subtracted one noisy figure from another. Allocation
// profiling cannot resolve it either — `-benchmem` counts the bitsets whether
// or not they are later dropped, so compacted and uncompacted report an
// identical B/op while differing in exactly the way that matters.
//
// The manifest is measured by heap sampling, which is reliable at its scale
// (repeat runs land within a fraction of a percent).
//
// The assertion is the premise, not a byte threshold. A threshold would fail
// on unrelated struct changes and train people to update the number rather
// than ask why it moved. What the decision rests on is the ratio: the
// manifest a terminal job has *already* released dwarfs everything compaction
// could reclaim. That is what should break if the premise stops holding.
func TestTerminalJobRetention_Measured(t *testing.T) {
	m := buildRetentionManifest(t)
	p := newJobProgress(m)

	manifestBytes := retainedManifestBytes(t)
	bitsetBytes := bitsetRetained(p.done) + bitsetRetained(p.failed) + bitsetRetained(p.emitted)

	t.Logf("%-42s %10.0f B/job  %7.3f B/article", "Manifest (evicted on terminal entry)",
		manifestBytes, manifestBytes/float64(retentionArticles))
	t.Logf("%-42s %10d B/job  %7.3f B/article", "per-article bitsets (what compaction drops)",
		bitsetBytes, float64(bitsetBytes)/float64(retentionArticles))
	t.Logf("compaction would reclaim %d B per parked job: %.0f parked jobs per MB, against a manifest %.0fx larger that is already gone",
		bitsetBytes, (1<<20)/float64(bitsetBytes), manifestBytes/float64(bitsetBytes))

	if bitsetBytes <= 0 {
		t.Fatalf("per-article bitsets retain %d B; the measurement is broken, not the design", bitsetBytes)
	}
	// docs/queue-lifecycle.md declines terminal compaction on the premise
	// that the already-evicted manifest is the dominant cost by orders of
	// magnitude. 50x is well below the ~183x measured, so this fails on a
	// real shift rather than on drift.
	if ratio := manifestBytes / float64(bitsetBytes); ratio < 50 {
		t.Errorf("manifest retains only %.0fx what compaction would reclaim (%.0f B vs %d B); docs/queue-lifecycle.md declines terminal compaction on the premise that this ratio is large, so revisit the decision rather than inheriting it",
			ratio, manifestBytes, bitsetBytes)
	}
}

// bitsetRetained returns the bytes a bitset's backing array holds. Exact:
// words is []uint64, so capacity times eight, plus nothing else that scales
// with article count.
func bitsetRetained(b bitset) int { return cap(b.words) * 8 }

// retainedManifestBytes reports the heap still held per manifest after a GC,
// averaged over retentionSamples live copies. Forcing a collection at both
// ends and averaging keeps allocator noise off a figure this large; it is
// indicative rather than exact, which is why the assertion above is a ratio
// with an order of magnitude of headroom.
func retainedManifestBytes(t *testing.T) float64 {
	t.Helper()
	keep := make([]*Manifest, 0, retentionSamples)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range retentionSamples {
		keep = append(keep, buildRetentionManifest(t))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	delta := float64(after.HeapAlloc) - float64(before.HeapAlloc)
	runtime.KeepAlive(keep)
	return delta / retentionSamples
}

func buildRetentionManifest(t *testing.T) *Manifest {
	t.Helper()
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {"retention"}},
		Groups: []string{"alt.binaries.test"},
		AvgAge: time.Unix(1700000000, 0),
	}
	for fi := range retentionFiles {
		f := nzb.File{Subject: fmt.Sprintf("retention file %d.rar", fi), Date: time.Unix(1700000000, 0)}
		for ai := range retentionPerFile {
			f.Articles = append(f.Articles, nzb.Article{
				ID:     fmt.Sprintf("<a-%d-%d@retention.example.com>", fi, ai),
				Bytes:  retentionArticleSize,
				Number: ai + 1,
			})
			f.Bytes += retentionArticleSize
		}
		parsed.Files = append(parsed.Files, f)
	}
	job, err := NewJob(parsed, AddOptions{Filename: "retention.nzb", Priority: constants.NormalPriority}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return m
}
