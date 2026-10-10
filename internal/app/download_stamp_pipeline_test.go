package app_test

import (
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/types"
)

// assessDelay is how long the Assessing worker is held in the first test. It
// is long enough to move a stamp taken after Assessing a whole history second
// past one taken at the download-complete report.
const assessDelay = 1500 * time.Millisecond

// TestDownloadFinish_IsStampedWhenTheJobLeavesFetching drives a job through the
// real pipeline to a history row with its Assessing worker delayed, as a job
// waiting behind others for a compute slot is. The finish is stamped before
// Assessing begins, so the delay is not download time: the history entry
// reports the time from the start, stamped 30 seconds back, to the finish
// already stamped when Assessing began, not that plus the delay, and not the
// one-second fallback for a missing stamp.
func TestDownloadFinish_IsStampedWhenTheJobLeavesFetching(t *testing.T) {
	t.Parallel()
	h := newScenarioHarness(t)

	var mu sync.Mutex
	var atAssess time.Time
	var jobRef *job.Job
	h.app.SetAssessHook(func(string) {
		mu.Lock()
		if jobRef != nil {
			atAssess = jobRef.DownloadFinished()
		}
		mu.Unlock()
		time.Sleep(assessDelay)
	})
	h.Start()

	// Paused so the start can be stamped before the first article decodes: the
	// start is first-wins, so a real article cannot move it afterwards.
	h.app.Dispatcher().Pause()
	j := h.AddSimpleJob("stamped", []byte("hello world payload"))
	mu.Lock()
	jobRef = j
	mu.Unlock()
	started := time.Now().Add(-30 * time.Second)
	if err := j.MarkJobStarted(started); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}
	h.app.Dispatcher().Resume()

	if !h.WaitForHistory(j.ID(), 30*time.Second) {
		t.Fatal("the job never reached history")
	}
	entry, err := h.repo.Get(t.Context(), j.ID())
	if err != nil {
		t.Fatalf("history Get: %v", err)
	}

	mu.Lock()
	seen := atAssess
	mu.Unlock()
	if seen.IsZero() {
		t.Error("the download finish was not stamped by the time Assessing began")
	}
	// The finish seen when Assessing began is the one history must report; a
	// stamp taken after the delay would put it assessDelay later.
	if want := int64(seen.Sub(started).Seconds()); entry.DownloadTime != want {
		t.Errorf("history DownloadTime = %d, want %d (the finish at the start of Assessing): the finish was stamped after the %v Assessing delay, or not at all (1 is the fallback)",
			entry.DownloadTime, want, assessDelay)
	}
}

// TestDemotionToFetching_RestampsTheDownloadFinish: on-demand par2 releases a
// recovery volume and Assessing sends the job back to Fetching. The finish it
// recorded on the way out is then wrong, and the job takes a new one when it
// leaves Fetching again: the finish seen at the second Assessing visit is a
// later one, not the first visit's kept by the first-wins slot.
func TestDemotionToFetching_RestampsTheDownloadFinish(t *testing.T) {
	t.Parallel()
	adminDir, downloadDir, completeDir, repo := setupTestDirsAndRepo(t)
	cfg := testConfig(downloadDir, completeDir, adminDir)
	cfg.With(func(c *config.Config) { c.Downloads.OnDemandPar2 = true })
	j, hdr := buildTestJob(t, cfg, deferredVolumeNZB("restamp"), types.FetchOptions{NzbName: "restamp", PP: types.PPRepair})
	persistCompleteJob(t, repo, adminDir, j, hdr, j.State(), 1)
	writePayload(t, downloadDir, j.Name())

	var mu sync.Mutex
	var visits []time.Time
	startRouteApp(t, repo, adminDir, downloadDir, completeDir, func(a *app.Application) {
		a.SetAssessHook(func(id string) {
			rj, ok := a.Dispatcher().Job(id)
			if !ok {
				return
			}
			mu.Lock()
			visits = append(visits, rj.DownloadFinished())
			mu.Unlock()
		})
	})

	var got []time.Time
	if !waitUntil(verdictWait, func() bool {
		mu.Lock()
		defer mu.Unlock()
		got = append(got[:0], visits...)
		return len(got) >= 2
	}) {
		t.Fatalf("the job reached Assessing %d time(s), want 2: the demotion and the return were not exercised", len(got))
	}
	if got[0].IsZero() {
		t.Fatal("no finish was stamped before the first Assessing visit")
	}
	if !got[1].After(got[0]) {
		t.Errorf("finish at the second Assessing visit = %v, want one after the first visit's %v: the demotion did not reopen the slot", got[1], got[0])
	}
}
