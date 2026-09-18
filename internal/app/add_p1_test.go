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
}

func buildTestIngestJob(t *testing.T, application *Application, parsed *nzb.NZB, name string) (*job.Job, dispatch.Header, []byte) {
	t.Helper()
	j, hdr, err := BuildIngestJob(application.config, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j, hdr, []byte("<nzb></nzb>")
}
