package queue

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// discardFixture adds the #287 on-demand par2 job (one content file, one par2
// index, one deferred recovery volume at index 2), discards the deferred
// volume, and runs one ordinary checkpoint.
//
// Every test below turns on the same premise as DiscardDeferredPar2's own
// doc comment: the discard does not change the file set, so nothing here is
// about staleness or a rebuild reaching disk. It is about the fetch policy
// itself surviving eviction, rehydration and a restart — and the discard has
// "no store write of its own", so the checkpoint is required, not optional:
// without it the policy has not reached job_files yet, and a snapshot taken
// before one runs would correctly read the pre-discard value back out of the
// store via RestoreJobProgress.
func discardFixture(t *testing.T, name string) (*Queue, *Job) {
	t.Helper()
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := addOnDemandPar2Job(t, q, name)

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return q, job
}

// TestDiscardDeferredPar2_ManifestSurvivesEviction replaces
// TestDiscardDeferredPar2_RewritesTheStoredManifest.
//
// The original pinned #294: a discard used to rebuild and shrink the
// manifest, and evicting the job before that rebuild reached disk resurrected
// the pre-discard shape, which #293's staleness guard then correctly refused
// to pair with the job's already-shrunk progress — leaving the job
// permanently un-hydratable. This task removes the rebuild itself, so there
// is nothing to race: the on-disk manifest Add wrote is already correct and
// discard never touches it. What is left to pin is that eviction and
// rehydration still agree with what Add wrote, and that the fetch policy
// (which does change, and does live on the permanently-resident JobProgress)
// is what actually reads back as FetchNever.
func TestDiscardDeferredPar2_ManifestSurvivesEviction(t *testing.T) {
	q, job := discardFixture(t, "discard-rewrites")

	before, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	beforeFiles := before.NumFiles()

	// Evicting drops the in-memory manifest, so the next read has to come
	// from disk.
	if err := q.SetStatus(job.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(Paused): %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: expected eviction to drop the manifest")
	}

	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	m, mErr := snap.Manifest()
	if mErr != nil {
		t.Fatalf("Manifest() = %v, want the untouched on-disk manifest to still be readable", mErr)
	}
	if m.NumFiles() != beforeFiles {
		t.Errorf("stored manifest has %d files, want %d — the file set must not shrink", m.NumFiles(), beforeFiles)
	}
	if got := snap.Progress().FileFetchPolicy(2); got != FetchNever {
		t.Errorf("file 2 policy = %d, want FetchNever", got)
	}
}

// TestDiscardDeferredPar2_HydratesBackIntoAResidentStatus is the same
// property reached through hydrateJobLocked rather than a snapshot clone.
// Promotion must bring a discarded job back with its file set intact.
func TestDiscardDeferredPar2_HydratesBackIntoAResidentStatus(t *testing.T) {
	q, job := discardFixture(t, "discard-rehydrates")

	before, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	beforeFiles := before.NumFiles()

	if err := q.SetStatus(job.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(Paused): %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: expected eviction")
	}

	if err := q.SetStatus(job.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus(Downloading) = %v, want the job to hydrate cleanly", err)
	}
	if !manifestResident(job) {
		t.Fatal("the job was not made resident")
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.NumFiles() != beforeFiles {
		t.Errorf("re-hydrated manifest has %d files, want %d", m.NumFiles(), beforeFiles)
	}
	if got := job.Progress().FileFetchPolicy(2); got != FetchNever {
		t.Errorf("file 2 policy = %d, want FetchNever after rehydration", got)
	}
}

// TestDiscardDeferredPar2_ReportsAPersistenceFailureAndKeepsTheRebuild used to
// pin what a failed Store.ReplaceManifest call during a discard did: the
// error had to reach the caller, and the in-memory rebuild had to survive it
// regardless. That premise is gone outright, not just changed — this task's
// whole point is that DiscardDeferredPar2 no longer calls ReplaceManifest, or
// any other store method, at all (see the interface note in the task brief).
// There is no store write for a discard to fail, so there is nothing left
// here to pin: DiscardDeferredPar2 looks the job up directly in q.byID, not
// through q.residentJob, so its only remaining failure mode is the job being
// genuinely absent (ErrNotFound) — ErrJobNotResident is unreachable from
// this method. That case is already covered directly, by
// TestDiscardDeferredPar2 in ondemand_par2_test.go, which asserts
// DiscardDeferredPar2("missing") returns an error. No replacement test is
// added in this file.

// TestDiscardDeferredPar2_ReplacesJobFilesRows used to pin that job_files rows
// were renumbered wholesale to match a shrunk, re-indexed manifest — the
// second half of #294. That is now the wrong invariant on purpose: this task
// exists specifically because renumbering job_files by a removed file's
// position is the root of #294/#308/#310/#315/#317. The replacement,
// TestDiscardDeferredPar2_RowsStayAtTheirIndices, pins the opposite: indices
// must NOT move, and the surviving file's stored progress must stay on its
// own row rather than travel onto the discarded file's old slot.
func TestDiscardDeferredPar2_RowsStayAtTheirIndices(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))

	// Place the deferred file in the middle so a renumbering bug (were one
	// reintroduced) would be visible: content-2 would shift from index 2 to
	// index 1.
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "content-1.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c1@x", Bytes: 1000, Number: 1}}},
			{Subject: `"content.vol000+01.par2" yEnc`, Bytes: 500, Articles: []nzb.Article{{ID: "v@x", Bytes: 500, Number: 1}}},
			{Subject: "content-2.bin", Bytes: 1000, Articles: []nzb.Article{{ID: "c2@x", Bytes: 1000, Number: 1}}},
		},
	}
	job, err := NewJob(parsed, AddOptions{Filename: "discard-rows.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if pre := mustManifest(t, job); !pre.FileIsPar2Recovery(1) {
		t.Fatalf("fixture guard: expected the deferred recovery volume at index 1")
	}
	if err := q.MarkArticlesDone(job.ID, []string{"c2@x"}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.NumFiles() != 3 {
		t.Fatalf("NumFiles = %d, want 3 — the file set must not shrink", m.NumFiles())
	}
	if m.FileSubject(2) != "content-2.bin" {
		t.Fatalf("content-2 moved to index %d; indices after a discarded file must stay put", 2)
	}
	if job.Progress().FileFetchPolicy(1) != FetchNever {
		t.Errorf("file 1 policy = %d, want FetchNever", job.Progress().FileFetchPolicy(1))
	}

	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	counts, err := store.ArticleCountsByJob(t.Context())
	if err != nil {
		t.Fatalf("ArticleCountsByJob: %v", err)
	}
	got := counts[job.ID]
	if len(got) != 3 {
		t.Fatalf("job_files holds %d rows, want 3 — the row set must not shrink", len(got))
	}
	if got[1].Fetch != FetchNever {
		t.Errorf("row 1 fetch policy = %d, want FetchNever", got[1].Fetch)
	}

	// Reloading must show content-2's downloaded article still attached to
	// its own row (index 2), not spliced onto the discarded volume's row —
	// which is what a renumbering bug would produce.
	reloaded, err := Load(dir, WithStore(store))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rj, err := reloaded.Get(job.ID)
	if err != nil {
		t.Fatalf("the job did not survive the reload: %v", err)
	}
	if rj.NumFiles() != 3 {
		t.Errorf("after a reload the job has %d files, want 3", rj.NumFiles())
	}
	if rj.Progress().FileFetchPolicy(1) != FetchNever {
		t.Errorf("after a reload file 1 policy = %d, want FetchNever", rj.Progress().FileFetchPolicy(1))
	}
	rsnap := reloaded.SnapshotJob(job.ID)
	if rsnap == nil {
		t.Fatal("SnapshotJob after reload returned nil")
	}
	rm, rErr := rsnap.Manifest()
	if rErr != nil {
		t.Fatalf("Manifest after reload: %v", rErr)
	}
	lo, _ := rm.FileRange(2)
	if !rsnap.Progress().ArticleDone(lo) {
		t.Error("content-2's downloaded article is not Done after the reload")
	}
	if rsnap.Progress().ArticleDone(0) {
		t.Error("content-1 reads as downloaded after the reload, but only content-2 was")
	}
}

// TestDiscardDeferredPar2_LostOnPromotionOfNonResidentJob pins the actual
// behaviour of a discard issued against a job that was never resident to
// begin with (StatusQueued, job.manifest == nil): the mark is in-memory
// only, and PromoteNext's unconditional rebuild —
// job.setResidency(&manifest, newJobProgress(&manifest)) when job.manifest
// is nil — throws it away. RestoreJobProgress then assigns the stale
// FetchIfNeeded straight from the still-unmodified job_files row.
//
// This is the opposite of what queue.go's DiscardDeferredPar2 doc comment
// and docs/queue-lifecycle.md used to claim: that the mark "stays
// in-memory-only... until the job is promoted back to resident and a
// checkpoint runs", implying it survives and eventually lands. It does not
// survive; it is lost outright. What makes that tolerable is that losing it
// makes HasDeferredPar2() true again, so maybeReleaseRecoveryVolumes simply
// redoes the CRC pass on the next completion event and re-discards — one
// redundant verification, not lost data or a stuck job. This test pins only
// the loss itself; the resident half of the round trip (discard survives
// restart while resident) is already covered by
// TestDiscardDeferredPar2_SurvivesRestart above.
func TestDiscardDeferredPar2_LostOnPromotionOfNonResidentJob(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Occupy the single active slot so jobB is added StatusQueued and never
	// becomes resident in the first place.
	filler := makeMultiFileJob(t, "lost-promo-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	jobB := addOnDemandPar2Job(t, q, "lost-promo-jobb")

	q.mu.RLock()
	b := q.byID[jobB.ID]
	q.mu.RUnlock()
	if b.Status != constants.StatusQueued || b.manifest != nil {
		t.Fatalf("fixture guard: jobB status=%v manifest!=nil=%v, want StatusQueued/nil manifest",
			b.Status, b.manifest != nil)
	}

	if err := q.DiscardDeferredPar2(jobB.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	if got := jobB.Progress().FileFetchPolicy(2); got != FetchNever {
		t.Fatalf("fixture guard: before promotion, policy = %d, want FetchNever(2)", got)
	}
	// Save so the persisted row is exactly what a real checkpoint would
	// leave for a non-resident job: unchanged, since updateTx gates the
	// whole job_files loop on job.Manifest() succeeding.
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Free the active slot and promote jobB — the exact path that rebuilds
	// JobProgress from scratch.
	q.SetMaxActiveJobs(2)

	q.mu.RLock()
	promoted := q.byID[jobB.ID]
	q.mu.RUnlock()
	if promoted.Status != constants.StatusDownloading {
		t.Fatalf("fixture guard: jobB.Status = %v after SetMaxActiveJobs, want StatusDownloading (not promoted)", promoted.Status)
	}

	// The discard is lost: the policy reverts to FetchIfNeeded rather than
	// staying FetchNever.
	if got := promoted.Progress().FileFetchPolicy(2); got != FetchIfNeeded {
		t.Errorf("after promotion, policy = %d, want FetchIfNeeded(1) — a discard on a non-resident "+
			"job must be lost on promotion, not carried forward silently wrong", got)
	}
	// The self-correcting half: losing the mark makes the volume look
	// deferred again, so maybeReleaseRecoveryVolumes has something to redo
	// rather than a job stuck believing a discard that never reached disk.
	if !promoted.HasDeferredPar2() {
		t.Error("after promotion, HasDeferredPar2() = false, want true — losing the discard mark must make the volume eligible for re-verification")
	}
}
