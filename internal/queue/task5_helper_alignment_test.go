package queue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
)

// This file exists to close check_test_alignment gaps that Task 5 of the
// derived-remaining-bytes refactor surfaced by touching job.go and queue.go
// for the first time in this branch (see docs/go-standards.md's gate
// semantics: touching any line of a file exposes every untested unexported
// helper in it, not just the changed lines). None of these helpers were
// changed by that refactor — they are pre-existing, and each test below
// exercises real behavior directly rather than a placeholder reference.

// TestSetScalarsFromManifest_CopiesFiveTotals pins setScalarsFromManifest's
// entire contract: it copies exactly the five manifest totals onto the job,
// nothing more and nothing less.
func TestSetScalarsFromManifest_CopiesFiveTotals(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "a.rar", Bytes: 1000, Articles: []JobArticle{{ID: "a1", Bytes: 1000}}},
		{Subject: "a.vol00+01.par2", Bytes: 500, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 500}}},
	})
	j := &Job{}
	j.setScalarsFromManifest(m)

	if got, want := j.TotalBytes(), m.TotalBytes(); got != want {
		t.Errorf("TotalBytes = %d, want %d", got, want)
	}
	if got, want := j.NumFiles(), m.NumFiles(); got != want {
		t.Errorf("NumFiles = %d, want %d", got, want)
	}
	if got, want := j.NumArticles(), m.NumArticles(); got != want {
		t.Errorf("NumArticles = %d, want %d", got, want)
	}
	if got, want := j.Par2Bytes(), m.Par2Bytes(); got != want {
		t.Errorf("Par2Bytes = %d, want %d", got, want)
	}
	if got, want := j.Par2Files(), m.Par2Files(); got != want {
		t.Errorf("Par2Files = %d, want %d", got, want)
	}
}

// TestSetAggregateScalarsFromFiles_LeavesPar2Untouched pins the contract
// documented on the method: totalBytes/numFiles/numArticles are set from the
// arguments, but par2Bytes/par2Files are deliberately left alone (they come
// from setPar2ScalarsFromStore instead).
func TestSetAggregateScalarsFromFiles_LeavesPar2Untouched(t *testing.T) {
	j := &Job{}
	j.setPar2ScalarsFromStore(999, 3) // pre-set to a sentinel this call must not disturb
	j.setAggregateScalarsFromFiles(12345, 4, 20)

	if got := j.TotalBytes(); got != 12345 {
		t.Errorf("TotalBytes = %d, want 12345", got)
	}
	if got := j.NumFiles(); got != 4 {
		t.Errorf("NumFiles = %d, want 4", got)
	}
	if got := j.NumArticles(); got != 20 {
		t.Errorf("NumArticles = %d, want 20", got)
	}
	if got := j.Par2Bytes(); got != 999 {
		t.Errorf("Par2Bytes = %d, want 999 (untouched)", got)
	}
	if got := j.Par2Files(); got != 3 {
		t.Errorf("Par2Files = %d, want 3 (untouched)", got)
	}
}

// TestSetPar2ScalarsFromStore_SetsOnlyThePair pins that it writes exactly
// par2Bytes/par2Files and nothing else.
func TestSetPar2ScalarsFromStore_SetsOnlyThePair(t *testing.T) {
	j := &Job{}
	j.setAggregateScalarsFromFiles(111, 2, 5)
	j.setPar2ScalarsFromStore(777, 1)

	if got := j.Par2Bytes(); got != 777 {
		t.Errorf("Par2Bytes = %d, want 777", got)
	}
	if got := j.Par2Files(); got != 1 {
		t.Errorf("Par2Files = %d, want 1", got)
	}
	if got := j.TotalBytes(); got != 111 {
		t.Errorf("TotalBytes = %d, want 111 (untouched)", got)
	}
}

// TestSetHydrateFailure_ManifestReturnsTheError pins setHydrateFailure's
// entire contract: it nils out the manifest, records progress, and stashes
// the error so a later Manifest() call surfaces it.
func TestSetHydrateFailure_ManifestReturnsTheError(t *testing.T) {
	m := newManifest([]JobFile{{Subject: "a", Articles: []JobArticle{{ID: "a1", Bytes: 1}}}})
	j := &Job{}
	j.manifest = m
	p := newJobProgress(m)
	wantErr := errors.New("boom: corrupt manifest")

	j.setHydrateFailure(p, wantErr)

	if _, err := j.Manifest(); !errors.Is(err, wantErr) {
		t.Errorf("Manifest() error = %v, want %v", err, wantErr)
	}
	if got := j.Progress(); got != p {
		t.Errorf("Progress() = %p, want %p (the progress passed in)", got, p)
	}
}

// TestResidentJob_ThreeOutcomes pins residentJob's three-way contract: not
// found, found but non-resident, and found and resident.
func TestResidentJob_ThreeOutcomes(t *testing.T) {
	q := New()

	if _, err := q.residentJob("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing job: err = %v, want ErrNotFound", err)
	}

	nonResident := &Job{ID: "j-nonresident", Status: constants.StatusQueued}
	q.byID["j-nonresident"] = nonResident
	if _, err := q.residentJob("j-nonresident"); !errors.Is(err, ErrJobNotResident) {
		t.Errorf("non-resident job: err = %v, want ErrJobNotResident", err)
	}

	resident := makeTestJob("j-resident", 1, 1)
	q.byID["j-resident"] = resident
	got, err := q.residentJob("j-resident")
	if err != nil {
		t.Fatalf("resident job: unexpected error %v", err)
	}
	if got != resident {
		t.Errorf("resident job: got %p, want %p", got, resident)
	}
}

// TestFindNextQueuedCandidateLocked_SkipsIneligible pins that the scan skips
// paused queues, resident/promoting/post-proc jobs, and picks the first
// eligible StatusQueued job in q.jobs order.
func TestFindNextQueuedCandidateLocked_SkipsIneligible(t *testing.T) {
	q := New()
	skip1 := makeTestJob("skip-resident", 1, 1)
	skip1.Status = constants.StatusQueued
	skip2 := makeTestJob("skip-postproc", 1, 1)
	skip2.Status = constants.StatusQueued
	skip2.PostProc = true
	skip3 := makeTestJob("skip-promoting", 1, 1)
	skip3.Status = constants.StatusQueued
	want := makeTestJob("eligible", 1, 1)
	want.Status = constants.StatusQueued

	q.jobs = []*Job{skip1, skip2, skip3, want}
	q.activeSet.Add(skip1) // resident, so IsResident(skip1.ID) is true
	q.promoting[skip3.ID] = true

	if got := q.findNextQueuedCandidateLocked(); got != want {
		t.Errorf("findNextQueuedCandidateLocked() = %v, want %v", got, want)
	}

	q.paused = true
	if got := q.findNextQueuedCandidateLocked(); got != nil {
		t.Errorf("findNextQueuedCandidateLocked() while paused = %v, want nil", got)
	}
}

// TestEvictJobLocked_ReleasesManifestKeepsProgress pins evictJobLocked's
// documented contract: only the manifest is released, progress stays
// resident.
func TestEvictJobLocked_ReleasesManifestKeepsProgress(t *testing.T) {
	q := New(WithStateDir(t.TempDir()))
	job := makeTestJob("evict-me", 1, 2)
	q.activeSet.Add(job)

	progressBefore := job.progress
	q.evictJobLocked(job)

	if job.manifest != nil {
		t.Error("manifest not released by evictJobLocked")
	}
	if job.progress != progressBefore {
		t.Error("progress was disturbed by evictJobLocked; must stay resident")
	}
	if q.activeSet.IsResident(job.ID) {
		t.Error("job still reported resident in ActiveSet after evictJobLocked")
	}

	// A nil job must be a no-op, not a panic.
	q.evictJobLocked(nil)
}

// recordingMoveStore is a minimal Store fake that records the entry passed
// to MoveToHistory, so finishClaimFailure can be exercised directly without
// a real SQLite backend.
type recordingMoveStore struct {
	Store
	movedJobID string
	movedEntry history.Entry
	moveCalled bool
}

func (s *recordingMoveStore) MoveToHistory(_ context.Context, job *Job, entry history.Entry) error {
	s.moveCalled = true
	s.movedJobID = job.ID
	s.movedEntry = entry
	return nil
}

// TestPrepareAndFinishClaimFailure_EvictsAndRenamesManifest pins the split
// contract: prepareClaimFailureLocked marks the job Failed and evicts it
// from RAM without doing I/O; finishClaimFailure then does the I/O (renaming
// the corrupt manifest file, moving the job to history) using what was
// captured.
func TestPrepareAndFinishClaimFailure_EvictsAndRenamesManifest(t *testing.T) {
	dir := t.TempDir()
	probe := &recordingMoveStore{}
	q := New(WithStore(probe), WithStateDir(dir))
	job := &Job{ID: "corrupt-job", Name: "corrupt", Status: constants.StatusQueued}
	q.byID[job.ID] = job
	q.jobs = []*Job{job}

	manifestPath := filepath.Join(dir, "manifests", job.ID+".json.gz")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("not valid gzip json"), 0o600); err != nil {
		t.Fatalf("write fake manifest: %v", err)
	}

	q.mu.Lock()
	cf := q.prepareClaimFailureLocked(job, manifestPath, errors.New("corrupt manifest bytes"))
	q.mu.Unlock()

	if job.Status != constants.StatusFailed {
		t.Errorf("job.Status = %v after prepareClaimFailureLocked, want StatusFailed", job.Status)
	}
	q.mu.RLock()
	_, stillPresent := q.byID[job.ID]
	q.mu.RUnlock()
	if stillPresent {
		t.Error("job still present in q.byID after prepareClaimFailureLocked")
	}

	q.finishClaimFailure(context.Background(), cf)

	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Errorf("original manifest path still exists after finishClaimFailure: err = %v", err)
	}
	if _, err := os.Stat(manifestPath + ".corrupt"); err != nil {
		t.Errorf("manifest not renamed to .corrupt: %v", err)
	}
	if !probe.moveCalled {
		t.Fatal("finishClaimFailure did not call MoveToHistory")
	}
	if probe.movedJobID != job.ID {
		t.Errorf("MoveToHistory job ID = %q, want %q", probe.movedJobID, job.ID)
	}
	if probe.movedEntry.Status != string(constants.StatusFailed) {
		t.Errorf("history entry Status = %q, want %q", probe.movedEntry.Status, constants.StatusFailed)
	}
}

// TestHydrateResidentLocked_HydratesForResidentStatus pins that a
// non-resident job gets its manifest loaded and joins the ActiveSet when
// transitioning into a resident status, and is a no-op for a
// non-resident-status target.
func TestHydrateResidentLocked_HydratesForResidentStatus(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))
	filler := makeMultiFileJob(t, "hydrate-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "hydrate-target", 1, 1)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	q.mu.Lock()
	target := q.byID[job.ID]
	if target.manifest != nil {
		q.mu.Unlock()
		t.Fatal("fixture guard: job still resident after Pause")
	}
	// A non-resident target status must no-op, leaving the job non-resident.
	if err := q.hydrateResidentLocked(target, job.ID, constants.StatusQueued, constants.StatusPaused); err != nil {
		q.mu.Unlock()
		t.Fatalf("hydrateResidentLocked(StatusQueued): %v", err)
	}
	if target.manifest != nil {
		q.mu.Unlock()
		t.Fatal("hydrateResidentLocked hydrated for a non-resident target status")
	}

	if err := q.hydrateResidentLocked(target, job.ID, constants.StatusDownloading, constants.StatusPaused); err != nil {
		q.mu.Unlock()
		t.Fatalf("hydrateResidentLocked(StatusDownloading): %v", err)
	}
	q.mu.Unlock()

	if target.manifest == nil {
		t.Error("hydrateResidentLocked did not hydrate the manifest for a resident target status")
	}
	if !q.activeSet.IsResident(job.ID) {
		t.Error("hydrateResidentLocked did not add the job to the ActiveSet")
	}
}

// TestUndeferRecoveryLocked_ClearsDeferredAndRecomputes pins its documented
// return value and side effects directly, distinct from the exported
// UndeferRecoveryVolumes wrapper's own end-to-end tests.
func TestUndeferRecoveryLocked_ClearsDeferredAndRecomputes(t *testing.T) {
	m := newManifest([]JobFile{
		{Subject: "content.bin", Bytes: 1000, Articles: []JobArticle{{ID: "c1", Bytes: 1000}}},
		{Subject: "content.vol00+01.par2", Bytes: 500, IsPar2Recovery: true, Articles: []JobArticle{{ID: "v1", Bytes: 500}}},
	})
	job := &Job{ID: "j1", Status: constants.StatusDownloading}
	job.manifest = m
	job.progress = newJobProgress(m)
	job.progress.files[1].Deferred = true
	job.progress.recompute(m)

	q := New()
	q.byID[job.ID] = job
	q.jobs = []*Job{job}

	q.mu.Lock()
	// Out-of-range and not-deferred indices must be ignored, not change anything.
	if changed := q.undeferRecoveryLocked(job, []int{99, 0}); changed {
		q.mu.Unlock()
		t.Fatal("undeferRecoveryLocked reported a change for out-of-range/non-deferred indices only")
	}
	changed := q.undeferRecoveryLocked(job, []int{1})
	q.mu.Unlock()

	if !changed {
		t.Fatal("undeferRecoveryLocked reported no change for a genuinely deferred index")
	}
	if job.progress.FileDeferred(1) {
		t.Error("file 1 still Deferred after undeferRecoveryLocked")
	}
	if !job.progress.Par2Recovered() {
		t.Error("Par2Recovered not set by undeferRecoveryLocked")
	}
	if got, want := job.progress.RemainingBytes(), int64(1500); got != want {
		t.Errorf("RemainingBytes = %d, want %d (recovery volume now counted)", got, want)
	}
}

// TestSetStatusLocked_ValidatesTransitions pins that setStatusLocked applies
// a legal transition and rejects an illegal one without mutating job.Status.
func TestSetStatusLocked_ValidatesTransitions(t *testing.T) {
	q := New()
	job := &Job{ID: "j1", Status: constants.StatusQueued}

	if err := q.setStatusLocked(job, constants.StatusDownloading); err != nil {
		t.Fatalf("legal transition Queued->Downloading: %v", err)
	}
	if job.Status != constants.StatusDownloading {
		t.Errorf("job.Status = %v, want StatusDownloading", job.Status)
	}

	job.Status = constants.StatusCompleted
	err := q.setStatusLocked(job, constants.StatusQueued)
	if !errors.Is(err, ErrIllegalStatusTransition) {
		t.Errorf("illegal transition Completed->Queued: err = %v, want ErrIllegalStatusTransition", err)
	}
	if job.Status != constants.StatusCompleted {
		t.Errorf("job.Status = %v after rejected transition, want unchanged StatusCompleted", job.Status)
	}
}
