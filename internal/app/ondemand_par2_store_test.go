package app

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/queue"
)

// storeBackedQueue builds the queue the daemon actually runs with: a SQLite
// store plus a state directory. TestMaybeReleaseRecoveryVolumes uses a bare
// queue.New(), which is why #287 went unnoticed — without a store nothing
// persists the deferred flag and without a state directory SnapshotJob never
// hydrates, so neither of the two paths that discarded the deferral could
// fire. The feature was covered only in the one configuration where it
// worked.
func storeBackedQueue(t *testing.T) *queue.Queue {
	t.Helper()
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	return queue.New(queue.WithStore(queue.NewSQLiteStore(repo.DB(), dir, repo)), queue.WithStateDir(dir))
}

// The on-demand par2 path, end to end, in the configuration production uses.
//
// maybeReleaseRecoveryVolumes returns early on !snap.HasDeferredPar2(), so
// with the deferral discarded the entire function was unreachable in any real
// deployment: the verification never ran, the volumes were never released,
// and they had already been downloaded with everything else — the opposite of
// what the feature promises.
func TestMaybeReleaseRecoveryVolumes_WithStore(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "store-par2"
	q := storeBackedQueue(t)
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "store-par2-name"
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	seedFileCRC(t, q, qjob, 0, 0x1068AFA6)

	// The precondition the whole feature rests on, asserted against a
	// snapshot because that is what maybeReleaseRecoveryVolumes receives.
	snap := q.SnapshotJob(jobID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if !snap.HasDeferredPar2() {
		t.Fatal("the snapshot reports no deferred par2 with a store configured; maybeReleaseRecoveryVolumes returns early and nothing below this line runs")
	}

	jobDir := filepath.Join(downloadDir, "store-par2-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")

	app := &Application{
		queue:   q,
		log:     slog.New(slog.DiscardHandler),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	// Data verifies clean, so the recovery volumes are not needed and must be
	// discarded rather than fetched — the saving the feature exists for.
	if app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
		t.Error("maybeReleaseRecoveryVolumes returned true on clean data; the recovery volumes would be downloaded for nothing")
	}
	after := q.SnapshotJob(jobID)
	m := mustManifest(t, after)
	sawRecovery := false
	for fi := range m.NumFiles() {
		if !m.FileIsPar2Recovery(fi) {
			continue
		}
		sawRecovery = true
		// DiscardDeferredPar2 no longer removes the file from the manifest
		// (see its doc comment in internal/queue/queue.go); it marks the
		// volume FetchNever and leaves it in place.
		if got := after.Progress().FileFetchPolicy(fi); got != queue.FetchNever {
			t.Errorf("recovery file %d policy = %d, want FetchNever after a clean verification", fi, got)
		}
	}
	if !sawRecovery {
		t.Fatal("fixture guard: expected a par2 recovery file to still be present in the manifest")
	}
}

// The other half: when the data is damaged the volumes must be un-deferred so
// they are fetched for repair. Also store-backed, for the same reason.
func TestMaybeReleaseRecoveryVolumes_WithStore_CorruptData(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "store-par2-corrupt"
	q := storeBackedQueue(t)
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "store-par2-corrupt-name"
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	seedFileCRC(t, q, qjob, 0, 0xDEADBEEF)

	snap := q.SnapshotJob(jobID)
	if snap == nil || !snap.HasDeferredPar2() {
		t.Fatal("fixture guard: the snapshot must arrive with a deferred volume or nothing below runs")
	}

	jobDir := filepath.Join(downloadDir, "store-par2-corrupt-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")

	app := &Application{
		queue:   q,
		log:     slog.New(slog.DiscardHandler),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	if !app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
		t.Fatal("maybeReleaseRecoveryVolumes returned false on damaged data; the recovery volumes needed for repair are never fetched")
	}
	after := q.SnapshotJob(jobID)
	if after.HasDeferredPar2() {
		t.Error("the recovery volume is still deferred after damage was detected, so repair will run without it")
	}
	if reason := after.Progress().Par2ReleaseReason(); reason == "" {
		t.Error("the volumes were released without recording why")
	}
}

// The unreadable-manifest branch, which #289 added and had to annotate as
// unreachable: the HasDeferredPar2 early return fired first, because #287
// discarded the deferral before any job got here.
//
// Two changes made it reachable. The deferral now survives into the snapshot,
// and a hydration failure now preserves the job's real progress instead of
// replacing it — so a job can arrive with its volumes still deferred and its
// manifest unreadable at once. That combination is the branch: the CRC
// comparison cannot be made, and finalizing would ship a download that may
// need a repair nothing checked for.
func TestMaybeReleaseRecoveryVolumes_UnreadableManifest(t *testing.T) {
	t.Parallel()
	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "store-par2-unreadable"
	stateDir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(stateDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	q := queue.New(
		queue.WithStore(queue.NewSQLiteStore(repo.DB(), stateDir, repo)),
		queue.WithStateDir(stateDir),
		queue.WithMaxActiveJobs(1),
	)

	// Fill the active slot so the job under test stays non-resident and
	// SnapshotJob has to hydrate it from the manifest we are about to break.
	filler := newPar2Job(t, []par2FileSpec{{subject: "filler.bin", bytes: 10}})
	filler.ID = "filler"
	filler.Name = "filler-name"
	if err := q.Add(filler); err != nil {
		t.Fatalf("Add filler: %v", err)
	}
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "store-par2-unreadable-name"
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	manifestPath := filepath.Join(stateDir, "manifests", jobID+".json.gz")
	if _, statErr := os.Stat(manifestPath); statErr != nil {
		t.Fatalf("fixture guard: no manifest written at %s: %v", manifestPath, statErr)
	}
	if err := os.WriteFile(manifestPath, []byte("not gzip json"), 0o600); err != nil {
		t.Fatalf("corrupt manifest: %v", err)
	}

	snap := q.SnapshotJob(jobID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if _, mErr := snap.Manifest(); mErr == nil {
		t.Fatal("fixture guard: the manifest read succeeded despite being corrupt")
	}
	if !snap.HasDeferredPar2() {
		t.Fatal("fixture guard: the deferral did not survive the failed hydration, so the early return fires and this branch is still unreachable")
	}

	var logBuf bytes.Buffer
	app := &Application{
		queue:   q,
		log:     slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		config:  cfg,
		emitter: dummyEmitter{},
	}

	// It reaches the branch and says so, but cannot carry the intent out.
	// An unreadable manifest is only observable for a non-resident job — a
	// resident one has its manifest in memory and never re-reads the file —
	// and UndeferRecoveryVolumes is manifest-tier, so the same non-residency
	// that produced the unreadable manifest also blocks the un-defer. The
	// function warns twice and finalizes without the recovery volumes.
	//
	// Asserting what happens rather than what was intended: the intent is
	// structurally unreachable here, tracked separately. A test written to
	// the comment's aspiration would have to be made to pass by changing the
	// assertion, which is how a test stops meaning anything.
	if got := app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap); got {
		t.Errorf("maybeReleaseRecoveryVolumes = true, want false: the un-defer cannot succeed while the job is non-resident, so reporting that volumes are being fetched would be a lie")
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "cannot verify without the manifest") {
		t.Errorf("the unverifiable manifest was not reported:\n%s", logged)
	}
	if !strings.Contains(logged, "un-defer failed") {
		t.Errorf("the failed un-defer was not reported, so finalizing without recovery volumes happens silently:\n%s", logged)
	}
}
