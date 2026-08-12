//go:build uitest

package uitest

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/queue"
)

// ackDone replaces the queue's deleted MarkArticleDone. It marks msgID
// durable via SeedFromExtents, the real resume path (see
// internal/queue/ackhelpers_test.go, the worked example for this
// migration), rebuilding the owning file's extent from the job's current
// durable bits plus the newly named article since SeedFromExtents installs
// a bitmap rather than merging one.
func ackDone(t *testing.T, q *queue.Queue, jobID, msgID string) {
	t.Helper()
	job, err := q.Get(jobID)
	if err != nil {
		t.Fatalf("ackDone: job %s not in queue: %v", jobID, err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("ackDone: job %s manifest: %v", jobID, err)
	}
	progress := job.Progress()

	target := -1
	for i := range m.NumArticles() {
		if m.ArticleID(i) == msgID {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatalf("ackDone: job %s has no article %s", jobID, msgID)
	}

	fi := -1
	var lo, hi int
	for f := range m.NumFiles() {
		l, h := m.FileRange(f)
		if target >= l && target < h {
			fi, lo, hi = f, l, h
			break
		}
	}
	if fi < 0 {
		t.Fatalf("ackDone: article %d not owned by any file in job %s", target, jobID)
	}

	bm := durability.NewBitmap(hi - lo)
	for i := lo; i < hi; i++ {
		if i == target || (progress.ArticleDone(i) && !progress.ArticleFailed(i)) {
			bm.Set(i - lo)
		}
	}
	ext := durability.FileExtent{
		FileIdx: int32(fi), //nolint:gosec // G115: file counts are far below int32
		Durable: bm,
	}
	if err := q.SeedFromExtents(jobID, []durability.FileExtent{ext}); err != nil {
		t.Fatalf("ackDone: SeedFromExtents: %v", err)
	}
}
