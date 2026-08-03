package app

import (
	"log/slog"
	"os"
	"path/filepath"
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
	if err := q.SetFileCRC32(jobID, 0, 0x1068AFA6); err != nil {
		t.Fatalf("SetFileCRC32: %v", err)
	}

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
	for fi := range m.NumFiles() {
		if m.FileIsPar2Recovery(fi) {
			t.Error("the deferred recovery volume is still on the job after a clean verification")
		}
	}
}

// The other half: when the data is damaged the volumes must be un-deferred so
// they are fetched for repair. Also store-backed, for the same reason.
func TestMaybeReleaseRecoveryVolumes_WithStore_CorruptData(t *testing.T) {
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
	if err := q.SetFileCRC32(jobID, 0, 0xDEADBEEF); err != nil {
		t.Fatalf("SetFileCRC32: %v", err)
	}

	snap := q.SnapshotJob(jobID)
	if snap == nil || !snap.HasDeferredPar2() {
		t.Fatal("fixture guard: the snapshot must arrive with a deferred volume or nothing below runs")
	}

	jobDir := filepath.Join(downloadDir, "store-par2-corrupt-name")
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)

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
