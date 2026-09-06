//go:build uitest

package uitest

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
)

// ackDone marks msgID durable via SeedFromRuns, the real resume path,
// handing it the single-article Run a barrier would have recorded.
func ackDone(t *testing.T, d *dispatch.Dispatcher, jobID, msgID string) {
	t.Helper()
	job, ok := d.Job(jobID)
	if !ok || job == nil {
		t.Fatalf("ackDone: job %s not in queue", jobID)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("ackDone: job %s manifest: %v", jobID, err)
	}

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
	for f := range m.NumFiles() {
		l, h := m.FileRange(f)
		if target >= l && target < h {
			fi = f
			break
		}
	}
	if fi < 0 {
		t.Fatalf("ackDone: article %d not owned by any file in job %s", target, jobID)
	}

	run := durability.Run{
		FileIdx:     int32(fi),     //nolint:gosec // G115: file counts are far below int32
		FirstArtIdx: int32(target), //nolint:gosec // G115: article counts are far below int32
		LastArtIdx:  int32(target), //nolint:gosec // G115: article counts are far below int32
		Length:      int64(m.ArticleBytes(target)),
	}
	if err := job.SeedFromRuns([]durability.Run{run}); err != nil {
		t.Fatalf("ackDone: SeedFromRuns: %v", err)
	}
}
