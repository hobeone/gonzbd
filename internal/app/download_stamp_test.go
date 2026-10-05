package app

import (
	"testing"
	"time"
)

// TestEnqueuePostProc_StampsTheDownloadFinishAndHistoryRecordsTheDuration: a job
// whose download began some seconds ago leaves the download phase with an
// ordered, non-zero start/finish pair, and its history entry reports the
// measured duration rather than the one-second fallback buildHistoryEntry uses
// when either stamp is missing or the duration truncates to 0.
func TestEnqueuePostProc_StampsTheDownloadFinishAndHistoryRecordsTheDuration(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, j := admittedApp(t, stage)
	id := j.ID()

	started := time.Now().Add(-30 * time.Second)
	if err := j.MarkJobStarted(started); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if !j.DownloadFinished().IsZero() {
		t.Fatal("the finish stamp was set before the job left the download phase")
	}

	application.maybeFinalize(id, "")
	awaitStage(t, stage.entered)

	gotStart, gotFinish := j.DownloadStarted(), j.DownloadFinished()
	if gotFinish.IsZero() {
		t.Fatal("download finish stamp is zero after the job was handed to post-processing")
	}
	if !gotStart.Equal(started) {
		t.Errorf("download start = %v, want the %v it was stamped with", gotStart, started)
	}
	if !gotFinish.After(gotStart) {
		t.Errorf("download finish %v is not after start %v", gotFinish, gotStart)
	}

	// A refused enqueue does not touch the stamp.
	row, ok := application.dispatcher.RowJob(j)
	if !ok {
		t.Fatal("the job is no longer registered")
	}
	application.enqueuePostProc(j, row.Header, "", false)
	if got := j.DownloadFinished(); !got.Equal(gotFinish) {
		t.Errorf("a refused second enqueue moved the finish stamp from %v to %v", gotFinish, got)
	}

	close(stage.finish)
	awaitFinalized(t, application, id)

	entry := historyEntry(t, application, id)
	if entry.DownloadTime < 29 || entry.DownloadTime > 60 {
		t.Errorf("history DownloadTime = %d, want about 30 (the 1 is the missing-stamp fallback)", entry.DownloadTime)
	}
}

// TestRetriedJob_RestampsTheDownloadFinish: ResetForRetry reopens both stamp
// slots, and the job's next admission to post-processing stamps the finish
// again rather than keeping the first run's. Production retries build a fresh
// Job, so this pins the reset and the stamp together, not that path.
func TestRetriedJob_RestampsTheDownloadFinish(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, j := admittedApp(t, stage)

	if err := j.MarkJobStarted(time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if err := j.MarkDownloadFinished(time.Now().Add(-30 * time.Second)); err != nil {
		t.Fatalf("MarkDownloadFinished: %v", err)
	}
	first := j.DownloadFinished()

	j.ResetForRetry()
	if !j.DownloadStarted().IsZero() || !j.DownloadFinished().IsZero() {
		t.Fatalf("ResetForRetry left stamps %v, %v", j.DownloadStarted(), j.DownloadFinished())
	}

	application.maybeFinalize(j.ID(), "")
	awaitStage(t, stage.entered)
	if got := j.DownloadFinished(); !got.After(first) {
		t.Errorf("retried job's finish stamp = %v, want one after the first run's %v", got, first)
	}
	close(stage.finish)
	awaitFinalized(t, application, j.ID())
}
