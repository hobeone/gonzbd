package queue

import (
	"fmt"
	"testing"
	"time"
	"unsafe"

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
)

// TestTerminalJobRetention_Measured is the evidence for declining terminal
// compaction (step 4 of docs/queue-lifecycle.md). It measures what a parked
// terminal job retains, and exists so the figures in that document can be
// re-derived rather than taken on trust.
//
// Both figures are computed from the backing arrays rather than sampled from
// the heap, which took two attempts to get right and is the reason this
// comment is long.
//
// `-benchmem` cannot answer the question at all: the per-article bitsets are
// allocated whether or not they are later dropped, so a compacted and an
// uncompacted JobProgress report an identical B/op while differing in exactly
// the way that matters. So the first draft sampled retained heap via
// runtime.MemStats. That draft reported a *negative* retained size for the
// compacted case and still passed, because the assertion subtracted one noisy
// figure from another and got a positive number — at 7.5 KB the saving sits
// under the allocator noise from building a 1.4 MB manifest.
//
// The second draft computed the bitsets exactly and sampled only the
// manifest, on the theory that 1.4 MB is far enough above the noise floor.
// That held when the test ran alone and failed inside the full package run,
// where other tests' parallel goroutines allocate concurrently and the
// HeapAlloc delta came back at a quarter of the true size. A measurement that
// depends on what else is running is not a measurement.
//
// Both are now derived from the structures themselves: exact, order-
// independent, and identical on every run. The lesson is that heap sampling
// answers "how much did the heap move", which is only the same question as
// "how much does this retain" when nothing else is happening.
//
// The assertion is the premise, not a byte threshold. A threshold would fail
// on unrelated struct changes and train people to update the number rather
// than ask why it moved. What the decision rests on is the ratio: the
// manifest a terminal job has *already* released dwarfs everything compaction
// could reclaim. That is what should break if the premise stops holding.
func TestTerminalJobRetention_Measured(t *testing.T) {
	t.Parallel()

	m := buildRetentionManifest(t)
	p := newJobProgress(m)

	manifestBytes := manifestRetained(m)
	bitsetBytes := bitsetRetained(p.done) + bitsetRetained(p.failed) + bitsetRetained(p.emitted)

	t.Logf("%-42s %10d B/job  %7.3f B/article", "Manifest (evicted on terminal entry)",
		manifestBytes, float64(manifestBytes)/float64(retentionArticles))
	t.Logf("%-42s %10d B/job  %7.3f B/article", "per-article bitsets (what compaction drops)",
		bitsetBytes, float64(bitsetBytes)/float64(retentionArticles))
	t.Logf("compaction would reclaim %d B per parked job: %.0f parked jobs per MB, against a manifest %.0fx larger that is already gone",
		bitsetBytes, (1<<20)/float64(bitsetBytes), float64(manifestBytes)/float64(bitsetBytes))

	if bitsetBytes <= 0 {
		t.Fatalf("per-article bitsets retain %d B; the measurement is broken, not the design", bitsetBytes)
	}
	// docs/queue-lifecycle.md declines terminal compaction on the premise
	// that the already-evicted manifest is the dominant cost by orders of
	// magnitude. 50x is well below the measured ratio, so this fails on a
	// real shift rather than on drift.
	if ratio := float64(manifestBytes) / float64(bitsetBytes); ratio < 50 {
		t.Errorf("manifest retains only %.0fx what compaction would reclaim (%d B vs %d B); docs/queue-lifecycle.md declines terminal compaction on the premise that this ratio is large, so revisit the decision rather than inheriting it",
			ratio, manifestBytes, bitsetBytes)
	}
}

// bitsetRetained returns the bytes a bitset's backing array holds. words is
// []uint64, so capacity times eight; nothing else in the struct scales with
// article count.
func bitsetRetained(b bitset) int { return cap(b.words) * 8 }

// manifestRetained returns the bytes a Manifest's backing arrays hold: the
// three parallel per-article arrays plus the article ID string data, the
// per-file slice and its subject strings, and the offsets prefix sum. It
// counts the slice payloads rather than the struct, which is the part that
// scales with the job and the part terminal eviction reclaims.
//
// messageIDIndex is excluded: it is built lazily and dropped by
// dropMessageIDIndex, so a parked job does not hold it. Counting it would
// overstate the very figure this test is used to defend.
func manifestRetained(m *Manifest) int {
	const (
		stringHeader = int(unsafe.Sizeof(""))
		intSize      = int(unsafe.Sizeof(int(0)))
	)
	total := cap(m.articleBytes)*intSize +
		cap(m.articleNumber)*intSize +
		cap(m.fileArticleOffsets)*intSize +
		cap(m.articleIDs)*stringHeader +
		cap(m.files)*int(unsafe.Sizeof(manifestFile{}))
	for _, id := range m.articleIDs {
		total += len(id)
	}
	for i := range m.files {
		total += len(m.files[i].subject)
	}
	return total
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
