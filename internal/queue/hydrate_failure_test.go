package queue

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
)

// A manifest that cannot be read is corruption, and must not look the same
// as a manifest that was merely evicted.
//
// hydrateSnapshot used to set residency only `if err == nil`, so a failed
// read left the clone's manifest nil and said nothing — no log, no error,
// no record on the job. Every consumer downstream then saw exactly what an
// evicted job looks like, and the four that dereference without a guard
// (par2 recovery, the postproc file listing, quickcheck) panicked or
// silently degraded on data loss they had no way to recognise.
func TestSnapshotJob_CorruptManifestIsReportedNotSilent(t *testing.T) {
	store, dir := setupResidencyTestStore(t)

	var logBuf bytes.Buffer
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1),
		WithLogger(slog.New(slog.NewTextHandler(&logBuf, nil))))

	// Fill the active slot so the job under test stays non-resident, then
	// corrupt the manifest it would hydrate from.
	filler := makeMultiFileJob(t, "hydrate-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "hydrate-corrupt", 2, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.mu.RLock()
	nonResident := q.byID[job.ID].manifest == nil
	q.mu.RUnlock()
	if !nonResident {
		t.Fatal("fixture guard: job is resident, hydration will not be attempted")
	}
	if err := fsutil.WriteGzAtomicBytes(dir+"/manifests/"+job.ID+".json.gz", []byte("not gzip json")); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if manifestResident(snap) {
		t.Fatal("fixture guard: manifest hydrated despite being corrupt")
	}

	if snap.hydrateErr == nil {
		t.Error("the clone records no hydration error; a consumer cannot tell this manifest is corrupt rather than evicted")
	}
	if logged := logBuf.String(); !strings.Contains(logged, job.ID) {
		t.Errorf("the read failure was not logged (log output: %q); a corrupt manifest must not pass in silence", logged)
	}
}

// The ordinary case must stay quiet: a job that hydrates cleanly records no
// error and logs no warning, or the signal above is worthless.
func TestSnapshotJob_SuccessfulHydrationIsQuiet(t *testing.T) {
	store, dir := setupResidencyTestStore(t)

	var logBuf bytes.Buffer
	q := New(WithStore(store), WithStateDir(dir), WithMaxActiveJobs(1),
		WithLogger(slog.New(slog.NewTextHandler(&logBuf, nil))))

	filler := makeMultiFileJob(t, "quiet-filler", 1, 1)
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	job := makeMultiFileJob(t, "quiet-ok", 2, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	snap := q.SnapshotJob(job.ID)
	if snap == nil || !manifestResident(snap) {
		t.Fatal("fixture guard: expected a successfully hydrated snapshot")
	}
	if snap.hydrateErr != nil {
		t.Errorf("hydrateErr = %v on a clean hydration, want nil", snap.hydrateErr)
	}
	if strings.Contains(logBuf.String(), "manifest") {
		t.Errorf("clean hydration logged about the manifest: %q", logBuf.String())
	}
}
