package queue

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestJobManifestProgressResidencyRace pins issue #263: Job.Manifest() and
// Job.Progress() are documented as safe to call without holding the queue
// lock, but a *Job returned by Queue.Get/List aliases queue storage, and
// evictJobLocked/PromoteNext/hydrateResidentLocked reassign job.manifest and
// job.progress under q.mu whenever the job's residency changes (Pause
// evicts; Resume + promotion re-hydrates).
//
// The fixture deliberately never lets the race window close: it flips the
// job between Downloading (resident) and Paused (evicted) on one goroutine
// while a second goroutine calls Manifest()/Progress() on the SAME *Job
// pointer the whole time, without ever taking q.mu itself. This is exactly
// the documented, intended usage pattern — a caller holding a *Job from
// Queue.Get() and calling the lock-free getters. Before the fix (raw field
// access, no per-Job lock) this trips `go test -race`; a job added and
// immediately promoted (rather than one that sits untouched after Add)
// keeps the pointer resident-then-evicted-then-resident in a tight loop so
// the writer side never goes quiet.
func TestJobManifestProgressResidencyRace(t *testing.T) {
	dir := t.TempDir()
	q := New(WithStateDir(dir))

	job := makeMultiFileJob(t, "residency-race", 2, 4)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Add's own PromoteNext call should have promoted the sole queued job to
	// Downloading (resident) immediately, since MaxActiveJobs defaults to 4.
	got, err := q.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != job {
		t.Fatal("fixture assumption violated: Get did not return the same *Job pointer passed to Add")
	}

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Reader: repeatedly calls the lock-free getters on the aliased pointer,
	// exactly as a caller outside the queue package is documented to be
	// allowed to do. Runs until the writer below signals it is done.
	wg.Go(func() {
		for !stop.Load() {
			_ = got.Manifest()
			_ = got.Progress()
		}
	})

	// Writer: repeatedly flips the job between resident (Downloading) and
	// evicted (Paused) states, which is what drives evictJobLocked and the
	// promotion re-hydration path to reassign job.manifest/job.progress.
	// Runs on this goroutine so it can signal the reader to stop as soon as
	// it finishes, rather than deadlocking on a WaitGroup the reader never
	// exits.
	for range 200 {
		if err := q.Pause(job.ID); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if err := q.Resume(job.ID); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	stop.Store(true)
	wg.Wait()
}

// TestJobScalarGettersRace pins the same property for the five
// manifest-derived scalar getters that Manifest()/Progress() already have.
// They are exported, they sit beside those two, and their doc comments
// ("never requires a resident manifest") invite exactly the lock-free
// external use those two document as safe — but the fields behind them are
// written by setScalarsFromManifest, which runs under q.mu on every
// promotion, on hydrateSnapshot, and on DiscardDeferredPar2's manifest
// rebuild. A caller outside this package cannot take q.mu, so without a
// per-Job lock the documented usage is a data race.
//
// No non-test caller reads these getters today — every consumer still goes
// through job.Manifest().TotalBytes() — so this races nothing in the
// current tree. That is precisely why it needs pinning now: wiring the
// consumers over to them is planned follow-up work, and it would introduce
// the race silently, with the race detector green until the first
// concurrent poll happened to land inside a promotion.
//
// Same fixture as above: Pause/Resume flips residency, and the promotion
// half of that loop re-runs setScalarsFromManifest each time.
func TestJobScalarGettersRace(t *testing.T) {
	dir := t.TempDir()
	q := New(WithStateDir(dir))

	job := makeMultiFileJob(t, "scalar-getter-race", 2, 4)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := q.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != job {
		t.Fatal("fixture assumption violated: Get did not return the same *Job pointer passed to Add")
	}

	// Guard against the fixture going vacuous: if the scalars were never
	// populated, a reader racing a writer that only ever assigns zero over
	// zero could still pass by accident.
	if got.TotalBytes() == 0 || got.NumArticles() == 0 {
		t.Fatalf("fixture assumption violated: scalars unset before the race (totalBytes=%d numArticles=%d)",
			got.TotalBytes(), got.NumArticles())
	}

	var stop atomic.Bool
	var wg sync.WaitGroup

	wg.Go(func() {
		for !stop.Load() {
			_ = got.TotalBytes()
			_ = got.NumFiles()
			_ = got.NumArticles()
			_ = got.Par2Bytes()
			_ = got.Par2Files()
		}
	})

	for range 200 {
		if err := q.Pause(job.ID); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if err := q.Resume(job.ID); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	stop.Store(true)
	wg.Wait()
}
