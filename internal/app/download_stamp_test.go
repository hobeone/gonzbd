package app

import (
	"testing"
	"time"
)

// TestEnqueuePostProc_FromFetching_StampsTheFinishWhenNoneIsSet: a job handed
// to post-processing while still at Fetching never makes the report that stamps
// the finish, so the hand-off stamps it, and history reports the measured
// duration rather than the one-second fallback.
func TestEnqueuePostProc_FromFetching_StampsTheFinishWhenNoneIsSet(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, j := admittedApp(t, stage)
	id := j.ID()

	started := time.Now().Add(-30 * time.Second)
	if err := j.MarkJobStarted(started); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	if !j.DownloadFinished().IsZero() {
		t.Fatal("fixture: the finish was stamped before the hand-off")
	}

	application.maybeFinalize(id, "")
	awaitStage(t, stage.entered)

	gotFinish := j.DownloadFinished()
	if gotFinish.IsZero() {
		t.Fatal("download finish stamp is zero after a hand-off from Fetching")
	}
	if !gotFinish.After(j.DownloadStarted()) {
		t.Errorf("download finish %v is not after start %v", gotFinish, j.DownloadStarted())
	}

	close(stage.finish)
	awaitFinalized(t, application, id)

	entry := historyEntry(t, application, id)
	if entry.DownloadTime < 29 || entry.DownloadTime > 31 {
		t.Errorf("history DownloadTime = %d, want about 30 (1 is the missing-stamp fallback)", entry.DownloadTime)
	}
}

// TestEnqueuePostProc_KeepsTheFinishAJobLeftFetchingWith: a job that left
// Fetching on its download-complete report already carries its finish, and the
// hand-over that follows keeps it.
func TestEnqueuePostProc_KeepsTheFinishAJobLeftFetchingWith(t *testing.T) {
	t.Parallel()
	stage := gatedStage{entered: make(chan string, 4), finish: make(chan struct{})}
	application, j := admittedApp(t, stage)

	if err := j.MarkJobStarted(time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	finish := time.Now().Add(-30 * time.Second)
	if err := j.MarkDownloadFinished(finish); err != nil {
		t.Fatalf("MarkDownloadFinished: %v", err)
	}

	application.maybeFinalize(j.ID(), "")
	awaitStage(t, stage.entered)
	if got := j.DownloadFinished(); !got.Equal(finish) {
		t.Errorf("the hand-over moved the finish from %v to %v", finish, got)
	}
	close(stage.finish)
	awaitFinalized(t, application, j.ID())
}
