package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	}); err != nil {
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
