package app_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestReload_NoArticleLossInFlight verifies that ReloadDownloader does not
// drop articles whose fetch was in-flight at swap time.
//
// Design: 6 files. File 0 is stalled on the NNTP server — a worker grabs
// it immediately and blocks. The other workers process files 1-5. Once the
// stall has fired (StallCount >= 1) AND files 1-5 are done, the test
// triggers a reload. The old downloader's context cancels the stalled
// fetch. ClearAllEmitted makes the article eligible again. The new
// downloader re-fetches it (stalls are one-shot).
func TestReload_NoArticleLossInFlight(t *testing.T) {
	const conns = 4
	h := newScenarioHarnessWithConns(t, conns)
	h.Start()

	const n = 6

	msgIDs := make([]string, 0, n)
	files := make([]nzb.File, 0, n)
	for i := range n {
		msgID := randomMsgID(t)
		msgIDs = append(msgIDs, msgID)
		raw := fmt.Appendf(nil, "content %d", i)
		h.server.AddArticle(msgID, yencSinglePart(fmt.Sprintf("file%d.bin", i), raw))

		files = append(files, nzb.File{
			Subject:  fmt.Sprintf(`"file%d.bin" yEnc (1/1)`, i),
			Articles: []nzb.Article{{ID: msgID, Bytes: len(raw), Number: 1}},
			Bytes:    int64(len(raw)),
		})
	}

	// Stall only file 0 — the first article dispatched. A worker grabs
	// it immediately and stalls. There's no risk of connWorker sem
	// contention blocking the stall because the stall fires on the
	// worker's first request (before sem is contended).
	h.InjectFailure(msgIDs[0], nntptest.FailureStall)

	parsed := &nzb.NZB{Files: files}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "reload-test"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := h.app.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Wait until the stall has fired on the server.
	if !h.WaitUntil(15*time.Second, func() bool {
		return h.server.StallCount() >= 1
	}) {
		t.Fatalf("precondition: stalls=%d, want >=1", h.server.StallCount())
	}

	// Trigger reload.
	cfg := h.server.ServerConfig("scenario", conns)
	if err := h.app.ReloadDownloader([]config.ServerConfig{cfg}); err != nil {
		t.Fatalf("ReloadDownloader: %v", err)
	}

	// Wait for the job to complete.
	if !h.WaitForPostProc(job.ID, 30*time.Second) {
		snap := h.app.Queue().SnapshotJob(job.ID)
		if snap != nil {
			m, p := snap.Manifest(), snap.Progress()
			if m != nil && p != nil {
				for fi := range m.NumFiles() {
					lo, hi := m.FileRange(fi)
					t.Logf("  file[%d]: complete=%v articles=%d", fi, p.FileComplete(fi), hi-lo)
					for i := lo; i < hi; i++ {
						t.Logf("    article[%d]: done=%v failed=%v emitted=%v id=%s",
							i-lo, p.ArticleDone(i), p.ArticleFailed(i), p.ArticleEmitted(i), m.ArticleID(i))
					}
				}
			}
		}
		t.Fatalf("job did not complete after reload")
	}

	// Assert invariants.
	if h.QueueContains(job.ID) {
		t.Errorf("job still in queue after completion")
	}
	hist, err := h.repo.Get(h.ctx, job.ID)
	if err != nil {
		t.Fatalf("history missing job: %v", err)
	}
	if hist.Status != "Completed" {
		t.Errorf("history status = %q, want Completed", hist.Status)
	}
}
