package app_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/config"
	"github.com/hobeone/sabnzbd-go/internal/nntp/nntptest"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

// TestReload_NoArticleLossInFlight verifies that ReloadDownloader does not
// drop articles whose ArticleResult had been emitted onto the completions
// channel but not yet consumed by the pipeline at swap time.
func TestReload_NoArticleLossInFlight(t *testing.T) {
	h := newScenarioHarness(t)
	h.Start()

	const n = 10
	var msgIDs []string
	var files []nzb.File
	for i := 0; i < n; i++ {
		msgID := randomMsgID(t)
		msgIDs = append(msgIDs, msgID)
		raw := []byte(fmt.Sprintf("content %d", i))
		h.server.AddArticle(msgID, yencSinglePart(fmt.Sprintf("file%d.bin", i), raw))

		files = append(files, nzb.File{
			Subject:  fmt.Sprintf(`"file%d.bin" yEnc (1/1)`, i),
			Articles: []nzb.Article{{ID: msgID, Bytes: len(raw), Number: 1}},
			Bytes:    int64(len(raw)),
		})
	}

	// Stall the first 2 articles of the second half (matching worker count).
	for i := n / 2; i < n/2+2; i++ {
		h.InjectFailure(msgIDs[i], nntptest.FailureStall)
	}

	parsed := &nzb.NZB{Files: files}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "reload-test"})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := h.app.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Wait until the first half are Done.
	h.WaitUntil(5*time.Second, func() bool {
		snap := h.app.Queue().SnapshotJob(job.ID)
		if snap == nil {
			return false
		}
		done := 0
		for _, f := range snap.Files {
			if f.Complete {
				done++
			}
		}
		return done >= n/2
	})

	// Trigger reload. The stalled workers for the second half will be
	// cancelled, and the new downloader will re-fetch them.
	cfg := h.server.ServerConfig("scenario", 2)
	if err := h.app.ReloadDownloader([]config.ServerConfig{cfg}); err != nil {
		t.Fatalf("ReloadDownloader: %v", err)
	}

	// Wait for the job to complete.
	if !h.WaitForPostProc(job.ID, 10*time.Second) {
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
