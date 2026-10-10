//go:build uitest

package uitest

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
)

// ackDone marks msgID Done via InstallVerified, the door a resumed job's
// verified written rows enter by, handing it the article's one row.
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

	lo, _ := m.FileRange(fi)
	var off int64
	for k := lo; k < target; k++ {
		off += int64(m.ArticleBytes(k))
	}
	row := durability.WrittenRow{
		FileIdx: fi,
		ArtIdx:  int32(target), //nolint:gosec // G115: article counts are far below int32
		Offset:  off,
		Length:  int64(m.ArticleBytes(target)),
	}
	if err := job.InstallVerified(fi, []durability.WrittenRow{row}); err != nil {
		t.Fatalf("ackDone: InstallVerified: %v", err)
	}
}
