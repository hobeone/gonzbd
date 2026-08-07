package queue

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
)

// makeMultiFileJobWithPar2 is makeMultiFileJob (lifecycle_test.go) plus one
// par2 recovery-volume file, so a test can assert on RecoveryBytes/RecoveryFiles
// — the two manifest scalars that travel through their own
// jobs.recovery_bytes/recovery_files columns (migration 010) rather than
// through SQLiteStore.Get's soft-failing job_files aggregate.
func makeMultiFileJobWithPar2(t *testing.T, name string, nFiles, nArticles int) *Job {
	t.Helper()
	parsed := &nzb.NZB{
		Meta:   map[string][]string{"title": {name}},
		Groups: []string{"alt.binaries.test"},
		AvgAge: time.Unix(1700000000, 0),
	}
	for fi := range nFiles {
		f := nzb.File{
			Subject: name + " - file " + string(rune('A'+fi)),
			Date:    time.Unix(1700000000, 0),
		}
		for ai := range nArticles {
			art := nzb.Article{
				ID:     articleID(fi, ai),
				Bytes:  100_000,
				Number: ai + 1,
			}
			f.Articles = append(f.Articles, art)
			f.Bytes += int64(art.Bytes)
		}
		parsed.Files = append(parsed.Files, f)
	}
	parsed.Files = append(parsed.Files, nzb.File{
		Subject:  name + ".vol01+02.par2",
		Date:     time.Unix(1700000000, 0),
		Bytes:    50_000,
		Articles: []nzb.Article{{ID: "par2-" + name + "@test", Bytes: 50_000, Number: 1}},
	})
	job, err := NewJob(parsed, AddOptions{
		Filename: name + ".nzb",
		Priority: constants.NormalPriority,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	return job
}

// The five manifest scalars must be readable from a job whose manifest has
// been evicted. That is the entire point of promoting them: a reporting
// path must never need a manifest.
func TestJobScalarsSurviveEviction(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	job := makeMultiFileJob(t, "scalars", 2, 3) // 2 files, 3 articles each
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	wantBytes := job.TotalBytes()
	if wantBytes == 0 {
		t.Fatal("fixture is useless: TotalBytes is zero while resident")
	}

	if err := q.Pause(job.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	q.mu.RLock()
	resident := q.byID[job.ID].manifest != nil
	q.mu.RUnlock()
	if resident {
		t.Fatal("fixture guard: job still resident after Pause, nothing is being tested")
	}

	if got := job.TotalBytes(); got != wantBytes {
		t.Errorf("TotalBytes after eviction = %d, want %d", got, wantBytes)
	}
	if got := job.NumFiles(); got != 2 {
		t.Errorf("NumFiles after eviction = %d, want 2", got)
	}
	if got := job.NumArticles(); got != 6 {
		t.Errorf("NumArticles after eviction = %d, want 6", got)
	}
}

// TestSnapshotJob_QueuedAfterRestart_ScalarsNotZero reproduces the gap named
// in the Task 3 review: a StatusQueued job restored via SQLiteStore.Get
// (through Loader.Load's store-backed branch) never has its manifest loaded
// by Get — that only happens for resident-status jobs — so the in-memory Job
// that comes back from a restart has zero scalars. Queue.SnapshotJob then
// clones that job and calls hydrateSnapshot, which reads the manifest from
// disk for other reasons (building progress) but, before this fix, never
// used it to backfill the five scalars. The result: an API response built
// from SnapshotJob reports zero size for a queued job that survived a
// restart, even though the manifest was in hand the whole time.
func TestSnapshotJob_QueuedAfterRestart_ScalarsNotZero(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Occupy the single active slot so the job under test stays queued and
	// non-resident even before the restart.
	filler := makeMultiFileJob(t, "filler-active", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}

	job := makeMultiFileJob(t, "queued-scalars", 2, 3) // 2 files, 3 articles each
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	wantBytes := job.TotalBytes()
	if wantBytes == 0 {
		t.Fatal("fixture is useless: TotalBytes is zero before restart")
	}

	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload a fresh Queue from the same store/dir — the real restart path.
	// q2 is a brand-new Queue with an empty byID; every job comes back via
	// store.List()/SQLiteStore.Get rather than any in-memory carry-over.
	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	q2.mu.RLock()
	restored, ok := q2.byID[job.ID]
	q2.mu.RUnlock()
	if !ok {
		t.Fatalf("job %s missing after reload", job.ID)
	}
	if restored.Status != constants.StatusQueued {
		t.Fatalf("fixture guard: job status after reload = %v, want StatusQueued", restored.Status)
	}
	if restored.manifest != nil {
		t.Fatal("fixture guard: job resident after reload, nothing is being tested")
	}

	snap := q2.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if got := snap.TotalBytes(); got != wantBytes {
		t.Errorf("SnapshotJob(...).TotalBytes() = %d, want %d", got, wantBytes)
	}
	if got := snap.NumFiles(); got != 2 {
		t.Errorf("SnapshotJob(...).NumFiles() = %d, want 2", got)
	}
	if got := snap.NumArticles(); got != 6 {
		t.Errorf("SnapshotJob(...).NumArticles() = %d, want 6", got)
	}
}

// TestPromoteNext_RestoredJobScalarsSynced covers the second zero-scalar
// site the Task 3 review flagged: PromoteNext attaches a manifest to a
// restored, non-resident StatusQueued job (job.manifest == nil) when
// promoting it to StatusDownloading, using its own manifest read rather than
// hydrateJobLocked's. Before the Task 3 fix, that attach path never
// backfilled the five scalars, so a job promoted after a restart reported
// zero size for the rest of its in-memory life — including while actively
// downloading.
//
// Task 4 additionally closed the sibling gap this test used to rely on as a
// fixture guard: SQLiteStore.Get now reconstructs TotalBytes/NumFiles/
// NumArticles for a non-resident job straight from job_files (bytes,
// COUNT(*), article_count), so a restored StatusQueued job is no longer
// zero even before PromoteNext runs. RecoveryBytes/RecoveryFiles are the one pair
// left that can tell "restored from the store" apart from "read from the
// resident manifest setScalarsFromManifest attaches at promotion", so
// asserting on them — zero before promotion, correct after — is what
// actually pins PromoteNext's own backfill call. TotalBytes/NumFiles/
// NumArticles alone would pass even if that call were deleted, since Get
// already supplies the right values for those three.
//
// The recovery figures now round-trip through its own columns, so a normally-saved job
// comes back with them already correct and that distinction disappears. The
// test zeroes the columns before reloading to recreate the unsynced state,
// which is the only thing left that PromoteNext's backfill can be observed
// changing. Deleting that step silently defeats this test.
func TestPromoteNext_RestoredJobScalarsSynced(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Occupy the single active slot so jobB stays queued and non-resident.
	filler := makeMultiFileJob(t, "promote-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	jobB := makeMultiFileJobWithPar2(t, "promote-scalars", 2, 3) // 2 regular files (3 articles each) + 1 par2 recovery volume
	if err := q.Add(jobB); err != nil {
		t.Fatalf("Add jobB: %v", err)
	}
	wantBytes := jobB.TotalBytes()
	wantRecoveryBytes := jobB.RecoveryBytes()
	wantRecoveryFiles := jobB.RecoveryFiles()
	if wantBytes == 0 {
		t.Fatal("fixture is useless: TotalBytes is zero before restart")
	}
	if wantRecoveryFiles == 0 || wantRecoveryBytes == 0 {
		t.Fatal("fixture is useless: RecoveryFiles/RecoveryBytes are zero before restart")
	}

	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// zero the recovery columns so the reloaded job arrives unsynced.
	//
	// This keeps the test's discriminator alive. Its whole design rests on
	// par2 being the one pair that reads zero for a non-resident job and
	// correct once a manifest is attached — that gap is what proves
	// PromoteNext calls setScalarsFromManifest, since the other three scalars
	// are already right from job_files and would pass even if the call were
	// deleted. Now that par2 round-trips through its own columns, a job saved
	// and reloaded normally comes back with them already correct, and the gap
	// closes. Recreating it here restores the discriminator: par2 must be
	// wrong before promotion and right after, or the backfill is unobservable.
	if _, err := store.db.ExecContext(t.Context(),
		"UPDATE jobs SET recovery_bytes = 0, recovery_files = 0 WHERE id = ?", jobB.ID); err != nil {
		t.Fatalf("blank recovery columns: %v", err)
	}

	// Reload — jobB comes back via SQLiteStore.Get non-resident (StatusQueued,
	// no manifest attached), but with TotalBytes/NumFiles/NumArticles already
	// reconstructed from job_files (the Task 4 gap closure), not zero.
	// RecoveryBytes/RecoveryFiles read zero because of the zeroing above.
	q2, err := Load(dir, WithStore(store), WithMaxActiveJobs(1))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	q2.mu.RLock()
	restored, ok := q2.byID[jobB.ID]
	q2.mu.RUnlock()
	if !ok {
		t.Fatalf("jobB missing after reload")
	}
	if restored.Status != constants.StatusQueued || restored.manifest != nil {
		t.Fatalf("fixture guard: jobB status=%v manifest!=nil=%v after reload, want StatusQueued/nil manifest",
			restored.Status, restored.manifest != nil)
	}
	if got := restored.TotalBytes(); got != wantBytes {
		t.Fatalf("fixture guard: jobB.TotalBytes() = %d before promotion, want %d (job_files reconstruction, see Task 4)", got, wantBytes)
	}
	if got := restored.RecoveryBytes(); got != 0 {
		t.Fatalf("fixture guard: jobB.RecoveryBytes() = %d before promotion, want 0 (zeroed above so the backfill is observable)", got)
	}
	if got := restored.RecoveryFiles(); got != 0 {
		t.Fatalf("fixture guard: jobB.RecoveryFiles() = %d before promotion, want 0 (zeroed above so the backfill is observable)", got)
	}

	// Free up the active slot and promote: this drives PromoteNext, the
	// exact path under test.
	q2.SetMaxActiveJobs(2)

	q2.mu.RLock()
	promoted := q2.byID[jobB.ID]
	q2.mu.RUnlock()
	if promoted.Status != constants.StatusDownloading {
		t.Fatalf("fixture guard: jobB.Status = %v after SetMaxActiveJobs, want StatusDownloading (not promoted)", promoted.Status)
	}

	if got := promoted.TotalBytes(); got != wantBytes {
		t.Errorf("TotalBytes after promotion = %d, want %d", got, wantBytes)
	}
	if got := promoted.NumFiles(); got != 3 {
		t.Errorf("NumFiles after promotion = %d, want 3", got)
	}
	if got := promoted.NumArticles(); got != 7 {
		t.Errorf("NumArticles after promotion = %d, want 7", got)
	}
	// The property that actually distinguishes "PromoteNext backfilled from
	// its manifest read" from "Get's job_files reconstruction carried
	// over": RecoveryBytes/RecoveryFiles must go from 0 to their real manifest
	// values only once the job is resident again.
	if got := promoted.RecoveryBytes(); got != wantRecoveryBytes {
		t.Errorf("RecoveryBytes after promotion = %d, want %d", got, wantRecoveryBytes)
	}
	if got := promoted.RecoveryFiles(); got != wantRecoveryFiles {
		t.Errorf("RecoveryFiles after promotion = %d, want %d", got, wantRecoveryFiles)
	}
}
