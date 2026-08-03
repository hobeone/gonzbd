package queue

import (
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
)

// deferredCount reports how many of the job's files are currently held back.
func deferredCount(job *Job) int {
	p := job.Progress()
	if p == nil {
		return 0
	}
	n := 0
	for i := range len(p.files) {
		if p.FileDeferred(i) {
			n++
		}
	}
	return n
}

// addOnDemandPar2Job builds the fixture from #287: one content file, one par2
// index, and one recovery volume, added with OnDemandPar2 so the recovery
// volume starts deferred.
func addOnDemandPar2Job(t *testing.T, q *Queue, name string) *Job {
	t.Helper()
	job, err := NewJob(par2NZB(), AddOptions{Filename: name + ".nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if deferredCount(job) != 1 {
		t.Fatalf("fixture guard: NewJob deferred %d files, want 1 — the rest of this test is about not losing that", deferredCount(job))
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return job
}

// Path 1 of #287: SQLiteStore.Add writes literal zeros for complete and
// deferred instead of the job's real per-file state, so the deferral is gone
// from SQLite the moment the job is stored. Promotion then rebuilds progress
// and RestoreJobProgress reads deferred = 0 back.
//
// A store is always configured in production, so this means on-demand par2
// defers nothing at all: the recovery volumes are downloaded along with
// everything else, which is precisely what the feature exists to avoid.
func TestOnDemandPar2_DeferralSurvivesTheStore(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := addOnDemandPar2Job(t, q, "store-deferral")

	if got := deferredCount(job); got != 1 {
		t.Errorf("after Add with a store the job defers %d files, want 1: the recovery volume will be downloaded despite OnDemandPar2", got)
	}
	if !job.HasDeferredPar2() {
		t.Error("HasDeferredPar2() is false after Add; maybeReleaseRecoveryVolumes returns early on this, making the whole on-demand path unreachable")
	}
}

// The same, read back through the store rather than from the live job, which
// is what promotion does.
func TestOnDemandPar2_DeferralRestoresFromTheStore(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := addOnDemandPar2Job(t, q, "store-restore")

	// Rebuild progress the way hydrateJobLocked does on promotion, then let
	// the store refill it. Anything the INSERT dropped is unrecoverable here.
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	fresh := newJobProgress(m)
	job.setResidency(m, fresh)
	if err := store.RestoreJobProgress(t.Context(), job); err != nil {
		t.Fatalf("RestoreJobProgress: %v", err)
	}

	if got := deferredCount(job); got != 1 {
		t.Errorf("after a store round-trip the job defers %d files, want 1: job_files.deferred was written as 0", got)
	}
}

// Path 2 of #287: hydrateSnapshot replaces the clone's progress with a fresh
// newJobProgress. That was harmless when a non-resident job had nil progress
// — there was nothing to lose — but #276 made JobProgress permanently
// resident, so the clone now arrives carrying real state and hydration
// discards it. The line did not change; its precondition did.
//
// Without a store nothing refills it, so the loss is total and visible
// directly.
func TestOnDemandPar2_SnapshotKeepsDeferralWithoutAStore(t *testing.T) {
	dir := t.TempDir()
	q := New(WithStateDir(dir), WithMaxActiveJobs(1))

	filler := makeMultiFileJob(t, "snap-defer-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := addOnDemandPar2Job(t, q, "snap-defer")
	if manifestResident(job) {
		t.Fatal("fixture guard: job is resident, so SnapshotJob will not hydrate and this exercises nothing")
	}
	if !job.HasDeferredPar2() {
		t.Fatal("fixture guard: the live job already lost its deferral before the snapshot")
	}

	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if got := deferredCount(snap); got != 1 {
		t.Errorf("the snapshot defers %d files while the live job defers %d: hydration rebuilt progress and threw the real state away",
			got, deferredCount(job))
	}
}

// A stale on-disk manifest must be reported, never paired with the job's
// progress.
//
// DiscardDeferredPar2 shrinks the manifest and progress in memory when
// verification comes back clean, and does not rewrite the .json.gz. Once the
// shrunk manifest is evicted, the file on disk still describes the
// pre-discard job. Preserving the live progress through hydration — which is
// what fixes #287 — makes that disagreement reachable, and handing the pair
// to recompute panics by design, on a background goroutine with no recover.
func TestStaleManifest_AfterDiscardIsReportedNotPanicked(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := addOnDemandPar2Job(t, q, "stale-discard")

	before, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	beforeFiles := before.NumFiles()
	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	after, err := job.Manifest()
	if err != nil {
		t.Fatalf("Manifest after discard: %v", err)
	}
	if after.NumFiles() >= beforeFiles {
		t.Fatalf("fixture guard: discard did not shrink the manifest (%d -> %d), so nothing below diverges",
			beforeFiles, after.NumFiles())
	}

	// Evicting drops the shrunk manifest and keeps the shrunk progress; the
	// larger pre-discard manifest is still the one on disk.
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
	_, mErr := snap.Manifest()
	if mErr == nil {
		t.Fatal("SnapshotJob served the stale on-disk manifest as if it described the job")
	}
	if !errors.Is(mErr, ErrManifestStale) {
		t.Errorf("Manifest() = %v, want an error satisfying errors.Is(err, ErrManifestStale)", mErr)
	}
	// The progress is the one thing here that is still true, so it must
	// survive rather than be replaced by the stale manifest's shape.
	if snap.Progress() == nil || len(snap.Progress().files) != after.NumFiles() {
		t.Error("the snapshot lost the job's real progress while refusing the stale manifest")
	}
}

// The same divergence reached through hydrateJobLocked, which acts on the
// live q.jobs entry rather than a clone. Promotion must fail closed, not
// crash the process.
func TestStaleManifest_HydrateJobLockedFailsClosed(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))
	job := addOnDemandPar2Job(t, q, "stale-hydrate")

	if err := q.DiscardDeferredPar2(job.ID); err != nil {
		t.Fatalf("DiscardDeferredPar2: %v", err)
	}
	if err := q.SetStatus(job.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(Paused): %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: expected eviction")
	}

	err := q.SetStatus(job.ID, constants.StatusDownloading)
	if err == nil {
		t.Fatal("SetStatus into a resident status succeeded against a stale manifest")
	}
	if !errors.Is(err, ErrManifestStale) {
		t.Errorf("SetStatus = %v, want an error satisfying errors.Is(err, ErrManifestStale)", err)
	}
	if manifestResident(job) {
		t.Error("the job was left resident with a manifest that contradicts its progress")
	}
}

// #287's third path: hydrateJobLocked rebuilt progress on its success path
// too, so promotion dropped the deferral on the live job. With a store this
// is masked by RestoreJobProgress re-reading the persisted column, so the
// no-store configuration is the one that shows it.
func TestOnDemandPar2_DeferralSurvivesPromotionWithoutAStore(t *testing.T) {
	dir := t.TempDir()
	q := New(WithStateDir(dir), WithMaxActiveJobs(1))

	filler := makeMultiFileJob(t, "promote-defer-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := addOnDemandPar2Job(t, q, "promote-defer")
	if manifestResident(job) {
		t.Fatal("fixture guard: job is resident, so promotion will not re-hydrate it")
	}
	if deferredCount(job) != 1 {
		t.Fatalf("fixture guard: deferral already lost at eviction (%d deferred)", deferredCount(job))
	}

	if err := q.SetStatus(job.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus(Downloading): %v", err)
	}
	if !manifestResident(job) {
		t.Fatal("fixture guard: expected the job to be hydrated")
	}
	if got := deferredCount(job); got != 1 {
		t.Errorf("after promotion the job defers %d files, want 1: hydrateJobLocked rebuilt progress and dropped the deferral", got)
	}
}
