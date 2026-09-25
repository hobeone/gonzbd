package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/checkpoint"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

type disconnectingSaveStore struct {
	dispatch.Store
	cancel context.CancelFunc
}

func (s disconnectingSaveStore) Save(ctx context.Context, p dispatch.Persisted) error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.Store.Save(ctx, p)
}

func TestAddJob_DisconnectDuringDispatcherSaveStillPersistsQueueRow(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d := dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		disconnectingSaveStore{Store: store.New(repo.DB()), cancel: cancel},
		application.runner,
	)
	application.dispatcher = d

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "p1-add.bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: "p1-add-0@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, rawNZB := buildTestIngestJob(t, application, parsed, "p1-add")

	if err := application.AddJob(ctx, j, hdr, rawNZB, false); err != nil {
		t.Fatalf("AddJob failed when caller disconnected during Save: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("caller context was never cancelled inside Save")
	}
	rows, err := store.New(repo.DB()).Load(t.Context())
	if err != nil {
		t.Fatalf("Load dispatch_jobs: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != j.ID() {
		t.Fatalf("dispatch_jobs rows = %+v, want 1 row for %s", rows, j.ID())
	}
}

func TestRetryHistoryJob_DisconnectDuringDispatcherSaveStillPersistsQueueRow(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d := dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		disconnectingSaveStore{Store: store.New(repo.DB()), cancel: cancel},
		application.runner,
	)
	application.dispatcher = d

	adminDir := application.config.GetGeneral().AdminDir
	nzbBackupDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbBackupDir, 0o750); err != nil {
		t.Fatalf("MkdirAll nzb backup: %v", err)
	}
	const jobID = "deadbeef12345678"
	const nzbBackup = "p1-retry.nzb.gz"
	rawNZB := []byte(`<?xml version="1.0" encoding="utf-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="1700000000" subject="p1-retry.bin yEnc (1/1)">
    <groups><group>alt.binaries.test</group></groups>
    <segments><segment bytes="100" number="1">p1-retry-0@t</segment></segments>
  </file>
</nzb>`)
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(nzbBackupDir, nzbBackup), rawNZB); err != nil {
		t.Fatalf("WriteGzAtomicBytes: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     jobID,
		Name:      "p1-retry",
		NzbName:   "p1-retry.nzb",
		NZBBackup: nzbBackup,
		Category:  "*",
		Status:    "Failed",
		Completed: time.Now(),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	if err := application.RetryHistoryJob(ctx, jobID); err != nil {
		t.Fatalf("RetryHistoryJob failed when caller disconnected during Save: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("caller context was never cancelled inside Save")
	}
	rows, err := store.New(repo.DB()).Load(t.Context())
	if err != nil {
		t.Fatalf("Load dispatch_jobs: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != jobID {
		t.Fatalf("dispatch_jobs rows = %+v, want 1 row for %s", rows, jobID)
	}

	// The history row must go under the same disconnect. history.nzo_id is
	// UNIQUE and the finalize path plain-INSERTs, so a survivor is not
	// overwritten later — this attempt's finalization fails to persist.
	var remaining int
	if err := repo.DB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM history WHERE nzo_id = ?", jobID).Scan(&remaining); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if remaining != 0 {
		t.Errorf("history rows for %s = %d after a retry whose caller disconnected, want 0; "+
			"the stale entry now blocks this attempt's finalization", jobID, remaining)
	}
}

func buildTestIngestJob(t *testing.T, application *Application, parsed *nzb.NZB, name string) (*job.Job, dispatch.Header, []byte) {
	t.Helper()
	j, hdr, err := BuildIngestJob(application.config, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j, hdr, []byte("<nzb></nzb>")
}

// failingSaveStore refuses every dispatch_jobs write, which is how a failed
// Dispatcher.Add is reached without racing anything.
type failingSaveStore struct {
	dispatch.Store
	err error
}

func (s failingSaveStore) Save(context.Context, dispatch.Persisted) error { return s.err }

// TestAddJob_FailedAddLeavesNoOrphanArtifacts pins that a job the dispatcher
// refused leaves nothing behind. The manifest, the NZB backup and the
// job_files rows are all written before Add is called, and Add unwinds its own
// registration on failure — so without this cleanup they are unowned: the only
// pass that walks them is built from dispatch_jobs, where the job has no row.
func TestAddJob_FailedAddLeavesNoOrphanArtifacts(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)

	application.dispatcher = dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		failingSaveStore{Store: store.New(repo.DB()), err: os.ErrPermission},
		application.runner,
	)

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "p1-orphan.bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: "p1-orphan-0@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, rawNZB := buildTestIngestJob(t, application, parsed, "p1-orphan")

	if err := application.AddJob(t.Context(), j, hdr, rawNZB, false); err == nil {
		t.Fatal("AddJob returned nil although the dispatcher's Save always fails")
	}

	adminDir := application.config.GetGeneral().AdminDir
	mpath, err := manifestPath(adminDir, j.ID())
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if _, err := os.Stat(mpath); !os.IsNotExist(err) {
		t.Errorf("manifest %s still on disk after a failed AddJob (stat err = %v)", mpath, err)
	}
	if hdr.NZBBackup != "" {
		backup := filepath.Join(adminDir, "nzb", filepath.Base(hdr.NZBBackup))
		if _, err := os.Stat(backup); !os.IsNotExist(err) {
			t.Errorf("NZB backup %s still on disk after a failed AddJob (stat err = %v)", backup, err)
		}
	}
	var n int
	if err := repo.DB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM job_files WHERE job_id = ?", j.ID()).Scan(&n); err != nil {
		t.Fatalf("count job_files: %v", err)
	}
	if n != 0 {
		t.Errorf("job_files rows for %s = %d after a failed AddJob, want 0", j.ID(), n)
	}
}

// TestDiscardUnaddedJobArtifacts_SurvivesRemovalFailures pins that cleanup is
// best effort. A manifest or backup path that cannot be removed is logged and
// stepped over: the caller is already returning the error that matters, and a
// cleanup that panicked or returned here would replace a real failure with a
// housekeeping one.
func TestDiscardUnaddedJobArtifacts_SurvivesRemovalFailures(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	adminDir := application.config.GetGeneral().AdminDir
	const jobID = "cafebabe0badf00d"

	// A non-empty directory where each file belongs: os.Remove refuses it with
	// something other than IsNotExist, which is the branch under test.
	mpath, err := manifestPath(adminDir, jobID)
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	backup := filepath.Join(adminDir, "nzb", "blocked.nzb.gz")
	for _, dir := range []string{mpath, backup} {
		if err := os.MkdirAll(filepath.Join(dir, "occupied"), 0o750); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}

	application.discardUnaddedJobArtifacts(t.Context(), jobID, "blocked.nzb.gz")

	for _, dir := range []string{mpath, backup} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s disappeared; cleanup was expected to fail and step over it: %v", dir, err)
		}
	}
}

// TestAddJob_FailedSeedLeavesNoOrphanArtifacts pins that the cleanup covers
// every failure after the first artifact is written, not only the dispatcher's.
// seedJobFiles runs after the NZB backup and the manifest, so a failure there
// used to return with both still on disk and nothing left to refer to them.
func TestAddJob_FailedSeedLeavesNoOrphanArtifacts(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)

	// Drop the table seedJobFiles writes, so it fails after the manifest and
	// the NZB backup are on disk.
	if _, err := repo.DB().ExecContext(t.Context(), "DROP TABLE job_files"); err != nil {
		t.Fatalf("drop job_files: %v", err)
	}

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject:  "p1-seedfail.bin",
		Bytes:    100,
		Articles: []nzb.Article{{ID: "p1-seedfail-0@t", Bytes: 100, Number: 1}},
	}}}
	j, hdr, rawNZB := buildTestIngestJob(t, application, parsed, "p1-seedfail")

	if err := application.AddJob(t.Context(), j, hdr, rawNZB, false); err == nil {
		t.Fatal("AddJob returned nil although job_files does not exist")
	}

	adminDir := application.config.GetGeneral().AdminDir
	mpath, err := manifestPath(adminDir, j.ID())
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if _, err := os.Stat(mpath); !os.IsNotExist(err) {
		t.Errorf("manifest %s survived a failed seed (stat err = %v)", mpath, err)
	}
}

// TestRetryHistoryJob_FailedAddRemovesTheQueueManifest pins that a retry the
// dispatcher refuses does not leave a queue manifest behind. A finalized job
// has none — jobFinalizer deletes it — so one written by a retry that never
// entered the queue is a file nothing refers to.
func TestRetryHistoryJob_FailedAddRemovesTheQueueManifest(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	application.dispatcher = dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		failingSaveStore{Store: store.New(repo.DB()), err: os.ErrPermission},
		application.runner,
	)

	adminDir := application.config.GetGeneral().AdminDir
	nzbBackupDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbBackupDir, 0o750); err != nil {
		t.Fatalf("MkdirAll nzb backup: %v", err)
	}
	const jobID = "feedface12345678"
	const nzbBackup = "p1-retry-fail.nzb.gz"
	rawNZB := []byte(`<?xml version="1.0" encoding="utf-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="1700000000" subject="p1-retry-fail.bin yEnc (1/1)">
    <groups><group>alt.binaries.test</group></groups>
    <segments><segment bytes="100" number="1">p1-retry-fail-0@t</segment></segments>
  </file>
</nzb>`)
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(nzbBackupDir, nzbBackup), rawNZB); err != nil {
		t.Fatalf("WriteGzAtomicBytes: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: jobID, Name: "p1-retry-fail", NzbName: "p1-retry-fail.nzb",
		NZBBackup: nzbBackup, Category: "*", Status: "Failed", Completed: time.Now(),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	if err := application.RetryHistoryJob(t.Context(), jobID); err == nil {
		t.Fatal("RetryHistoryJob returned nil although the dispatcher's Save always fails")
	}

	mpath, err := manifestPath(adminDir, jobID)
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if _, err := os.Stat(mpath); !os.IsNotExist(err) {
		t.Errorf("queue manifest %s survived a retry that never entered the queue (stat err = %v)", mpath, err)
	}
	// The retry seeded job_files before its Add failed. The entry is still
	// FAILED, and a failed entry keeps its runs and nothing else.
	var n int
	if err := repo.DB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM job_files WHERE job_id = ?", jobID).Scan(&n); err != nil {
		t.Fatalf("count job_files: %v", err)
	}
	if n != 0 {
		t.Errorf("job_files rows = %d after a retry that never entered the queue, want 0", n)
	}
	// The backup belongs to the history entry, which is still there.
	if _, err := os.Stat(filepath.Join(nzbBackupDir, nzbBackup)); err != nil {
		t.Errorf("NZB backup was removed although the history entry still owns it: %v", err)
	}
}

// TestRemoveNZBBackupIn_MissingDirectoryIsNotAnError pins the quiet half of
// the confined delete: AddJob writes no backup when the NZB had no filename,
// and on that path admin/nzb need not exist at all. An absent directory is the
// artifact already being gone, not a failure to report.
func TestRemoveNZBBackupIn_MissingDirectoryIsNotAnError(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	missing := filepath.Join(application.config.GetGeneral().AdminDir, "no-such-nzb-dir")

	application.removeNZBBackupIn(missing, "cafef00d00000001", "whatever.nzb.gz")

	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the missing directory was created by a delete (stat err = %v)", err)
	}
}

// failingSaveBatchStore makes checkpointer.Flush fail, which is the last of
// RetryHistoryJob's steps between writing the queue manifest and reaching
// dispatcher.Add.
type failingSaveBatchStore struct{ err error }

func (s failingSaveBatchStore) SaveBatch(context.Context, []job.Checkpoint) error { return s.err }

// TestRetryHistoryJob_FailedFlushRemovesTheQueueManifest covers the steps
// BETWEEN the manifest write and dispatcher.Add.
//
// TestRetryHistoryJob_FailedAddRemovesTheQueueManifest pins the Add itself,
// and pinning only that was the mistake: the rule is "a retry that never
// enters the queue leaves no queue manifest", and it governs every return
// between the write and the admission, not the one that prompted the fix.
// seedJobFiles and checkpointer.Flush both sit in that span. Flush is the
// reachable one here — it takes an injected store, where seedJobFiles goes
// straight to the history DB this app is otherwise using.
func TestRetryHistoryJob_FailedFlushRemovesTheQueueManifest(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	application.checkpointer = checkpoint.New(
		failingSaveBatchStore{err: os.ErrPermission}, time.Hour, application.log)

	adminDir := application.config.GetGeneral().AdminDir
	nzbBackupDir := filepath.Join(adminDir, "nzb")
	if err := os.MkdirAll(nzbBackupDir, 0o750); err != nil {
		t.Fatalf("MkdirAll nzb backup: %v", err)
	}
	const jobID = "feedface87654321"
	const nzbBackup = "p1-retry-flush.nzb.gz"
	rawNZB := []byte(`<?xml version="1.0" encoding="utf-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
  <file poster="test" date="1700000000" subject="p1-retry-flush.bin yEnc (1/1)">
    <groups><group>alt.binaries.test</group></groups>
    <segments><segment bytes="100" number="1">p1-retry-flush-0@t</segment></segments>
  </file>
</nzb>`)
	if err := fsutil.WriteGzAtomicBytes(filepath.Join(nzbBackupDir, nzbBackup), rawNZB); err != nil {
		t.Fatalf("WriteGzAtomicBytes: %v", err)
	}
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: jobID, Name: "p1-retry-flush", NzbName: "p1-retry-flush.nzb",
		NZBBackup: nzbBackup, Category: "*", Status: "Failed", Completed: time.Now(),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	if err := application.RetryHistoryJob(t.Context(), jobID); err == nil {
		t.Fatal("RetryHistoryJob returned nil although the checkpointer's Flush always fails")
	}

	mpath, err := manifestPath(adminDir, jobID)
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	if _, err := os.Stat(mpath); !os.IsNotExist(err) {
		t.Errorf("queue manifest %s survived a retry that never entered the queue (stat err = %v)", mpath, err)
	}
	// The backup belongs to the history entry, which is still there.
	if _, err := os.Stat(filepath.Join(nzbBackupDir, nzbBackup)); err != nil {
		t.Errorf("NZB backup was removed although the history entry still owns it: %v", err)
	}
}
