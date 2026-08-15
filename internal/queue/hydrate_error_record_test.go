package queue

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// hydrateJobLocked is the other production path that loads a manifest off
// disk, and it is the one that operates on the live q.jobs entry rather than
// on a detached clone. If it does not record why hydration failed, the
// distinction Manifest() promises exists only for SnapshotJob: after a failed
// SetStatus/Retry every later Manifest() call on that job reports the generic
// ErrJobNotResident, which is what an ordinary evicted job reports. A reader
// then cannot tell routine eviction from data loss — the exact confusion
// ErrJobNotResident was introduced to end.
func TestHydrateJobLocked_CorruptManifestRecordsWhy(t *testing.T) {
	store, dir := setupResidencyTestStore(t)
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1))

	// Fill the active slot so the job under test is evicted, then corrupt the
	// manifest that hydration will read.
	filler := makeMultiFileJob(t, "hjl-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "hjl-corrupt", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: job is resident, hydration will not be attempted")
	}
	if err := fsutil.WriteGzAtomicBytes(dir+"/manifests/"+job.ID+".json.gz", []byte("not gzip json")); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	// SetStatus into a resident status is what drives hydrateJobLocked.
	if err := q.SetStatus(job.ID, constants.StatusDownloading); err == nil {
		t.Fatal("fixture guard: SetStatus succeeded despite a corrupt manifest")
	}

	_, err := job.Manifest()
	if err == nil {
		t.Fatal("Manifest() returned no error for a job whose manifest is corrupt")
	}
	if errors.Is(err, ErrJobNotResident) {
		t.Errorf("Manifest() = %v, which reports routine eviction; a corrupt manifest is data loss and must not be indistinguishable from it", err)
	}
}

// The second failure branch of hydrateJobLocked: the manifest read succeeds
// but restoring progress from the store does not. The job is left
// non-resident by design, so the same reasoning applies — the reason has to
// travel with the job.
func TestHydrateJobLocked_RestoreFailureRecordsWhy(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real}
	q := New(WithStore(fs), WithStateDir(dir), WithMaxActiveJobs(1))

	filler := makeMultiFileJob(t, "hjl-restore-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "hjl-restore", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: job is resident, hydration will not be attempted")
	}

	fs.failRestore = true
	if err := q.SetStatus(job.ID, constants.StatusDownloading); err == nil {
		t.Fatal("fixture guard: SetStatus succeeded despite a failing store")
	}

	_, err := job.Manifest()
	if err == nil {
		t.Fatal("Manifest() returned no error after a failed hydration")
	}
	if errors.Is(err, ErrJobNotResident) {
		t.Errorf("Manifest() = %v, which reports routine eviction; a failed progress restore is not routine", err)
	}
}

// hydrateSnapshot overwrites the clone's real progress with an all-zero one
// before asking the store to fill it in. Discarding that call's error leaves
// the zero progress in place and returns it as if it were the job's true
// state — a part-downloaded job presented as having downloaded nothing, to
// every SnapshotJob consumer at once (par2 recovery, the pipeline's
// registerFile, DirectUnpack, the API per-file listing). hydrateJobLocked
// fails closed on this identical call; the two paths must not disagree.
func TestSnapshotJob_RestoreFailureIsNotSilent(t *testing.T) {
	real, dir := setupResidencyTestStore(t)
	fs := &failingStore{Store: real}

	var logBuf bytes.Buffer
	q := New(WithStore(fs), WithStateDir(dir), WithMaxActiveJobs(1),
		WithLogger(slog.New(slog.NewTextHandler(&logBuf, nil))))

	job := makeMultiFileJob(t, "snap-restore", 1, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Give the job real progress while it is still resident — MarkArticlesDone
	// resolves message IDs through the manifest — so that an all-zero
	// replacement is detectable as something other than its genuine state.
	ackDone(t, q, job.ID, articleID(0, 0))
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Pausing is a non-resident status, so this evicts the manifest while
	// leaving progress resident.
	if err := q.SetStatus(job.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(Paused): %v", err)
	}
	if manifestResident(job) {
		t.Fatal("fixture guard: job is resident, hydration will not be attempted")
	}
	if p := job.Progress(); p == nil || !p.ArticleDone(0) {
		t.Fatal("fixture guard: the live job should retain its downloaded article after eviction")
	}

	fs.failRestore = true
	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}

	if snap.hydrateErr == nil {
		t.Error("the clone records no error after a failed progress restore; a consumer cannot tell this from a routine eviction")
	}
	if logged := logBuf.String(); !strings.Contains(logged, job.ID) {
		t.Errorf("the restore failure was not logged (log output: %q)", logged)
	}
	if p := snap.Progress(); p == nil || !p.ArticleDone(0) {
		t.Error("the snapshot reports the downloaded article as not downloaded; the all-zero progress built for the store to fill was returned as the job's real state")
	}
}
