package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// newRemoveJobTestApp builds an Application wired for RemoveJob tests:
// a fresh download/admin dir pair, an open history repo, and default config.
func newRemoveJobTestApp(t *testing.T) *Application {
	t.Helper()

	dir := t.TempDir()
	downloadDir := filepath.Join(dir, "download")
	adminDir := filepath.Join(dir, "admin")
	if err := os.MkdirAll(downloadDir, 0o750); err != nil {
		t.Fatalf("create download directory: %v", err)
	}
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		t.Fatalf("create admin directory: %v", err)
	}

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("open history database: %v", err)
	}
	repo := history.NewRepository(db)
	cfg := testConfig(
		downloadDir,
		filepath.Join(dir, "complete"),
		adminDir,
		config.ServerConfig{Name: "test"},
	)
	a, err := New(cfg, repo)
	if err != nil {
		t.Fatalf("New application failed: %v", err)
	}
	return a
}

func TestRemoveJob(t *testing.T) {
	t.Parallel()
	a := newRemoveJobTestApp(t)
	downloadDir := a.config.GetGeneral().DownloadDir

	parsed := &nzb.NZB{}
	j, hdr, _ := BuildIngestJob(a.config, parsed, "to-delete.nzb", types.FetchOptions{NzbName: "to-delete"}, nil)
	_ = a.Dispatcher().Add(j, hdr)

	// Create a dummy download directory
	jobDir := filepath.Join(downloadDir, "to-delete")
	_ = os.MkdirAll(jobDir, 0o750)
	dummyFile := filepath.Join(jobDir, "data.bin")
	_ = os.WriteFile(dummyFile, []byte("data"), 0o600)

	// 1. Remove with deleteFiles=true
	err := a.RemoveJob(t.Context(), j.ID(), true)
	if err != nil {
		t.Fatalf("RemoveJob failed: %v", err)
	}
	if a.Dispatcher().Len() != 0 {
		t.Errorf("dispatcher len = %d, want 0", a.Dispatcher().Len())
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("job directory was NOT deleted but should have been")
	}

	// 2. Add again and remove
	j2, hdr2, _ := BuildIngestJob(a.config, parsed, "to-delete-files.nzb", types.FetchOptions{NzbName: "to-delete-files"}, nil)
	_ = a.Dispatcher().Add(j2, hdr2)
	jobDir2 := filepath.Join(downloadDir, "to-delete-files")
	_ = os.MkdirAll(jobDir2, 0o750)

	err = a.RemoveJob(t.Context(), j2.ID(), false)
	if err != nil {
		t.Fatalf("RemoveJob failed: %v", err)
	}
	if _, err := os.Stat(jobDir2); os.IsNotExist(err) {
		t.Errorf("job directory was deleted but should have been kept (deleteFiles=false)")
	}
}

// TestRemoveJob_NilDownloader pins the nil-guard on RemoveJob's final
// DisconnectAll call. New() always wires a real downloader, so app.downloader
// is nilled out directly (white-box, same package) after construction to
// exercise the branch.
func TestRemoveJob_NilDownloader(t *testing.T) {
	t.Parallel()
	a := newRemoveJobTestApp(t)
	a.downloader = nil

	parsed := &nzb.NZB{}
	j, hdr, _ := BuildIngestJob(a.config, parsed, "to-delete.nzb", types.FetchOptions{NzbName: "to-delete"}, nil)
	_ = a.Dispatcher().Add(j, hdr)

	if err := a.RemoveJob(t.Context(), j.ID(), false); err != nil {
		t.Fatalf("RemoveJob with nil downloader: %v", err)
	}
}

// errStoreDeleteRefused is the injected store-level failure.
var errStoreDeleteRefused = errors.New("store refused the delete")

// removeRefusingStore fails Remove and delegates everything else to a real store.
type removeRefusingStore struct {
	dispatch.Store
}

func (removeRefusingStore) Delete(context.Context, string) error {
	return errStoreDeleteRefused
}

// TestRemoveJob_StoreDeleteFailureLeavesTheJobsFilesOnDisk pins the ordering
// #376 is about: the step that can fail must run before the step that cannot
// be undone.
func TestRemoveJob_StoreDeleteFailureLeavesTheJobsFilesOnDisk(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	ctx := t.Context()

	refusing := removeRefusingStore{Store: store.New(repo.DB())}
	d := dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		refusing,
		application.runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	application.runner.report = d

	j, path := removeJobFixture(t, application, "refused")

	err := application.RemoveJob(ctx, j.ID(), true)
	if !errors.Is(err, errStoreDeleteRefused) {
		t.Fatalf("RemoveJob err = %v, want it to propagate %v", err, errStoreDeleteRefused)
	}
	if _, ok := application.Dispatcher().Job(j.ID()); !ok {
		t.Error("the job left the dispatcher although its store delete failed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the job's file is gone after a failed RemoveJob (stat: %v)", err)
	}
}

// disconnectingStore cancels a context at the instant the real store delete
// succeeds. That instant is the reachable half of #549: past it the job is
// gone from dispatch_jobs, so nothing will read its durability rows again and
// nothing will remove them either.
//
// Deterministic rather than timing-dependent, and the reason is a property of
// Dispatcher.Remove rather than of this type: Remove does not re-check its
// context after store.Delete returns (registry.go), so a cancellation landing
// here cannot make Remove fail and send RemoveJob down its early return. It
// lands in the window and only in the window, on every run.
type disconnectingStore struct {
	dispatch.Store
	cancel context.CancelFunc
}

func (s disconnectingStore) Delete(ctx context.Context, id string) error {
	if err := s.Store.Delete(ctx, id); err != nil {
		return err
	}
	s.cancel()
	return nil
}

// lockedBuffer is a slog destination safe to read while background goroutines
// are still writing to it. app.log is shared with the dispatcher and pipeline,
// so a bare bytes.Buffer here is a data race under -race rather than a
// theoretical one.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRemoveJob_DisconnectAfterDispatcherRemoveStillClearsDurability pins the
// detachment. RemoveJob is handed r.Context() by api/queue.go, so the context
// this cancels is exactly the one a closed browser tab cancels.
//
// Without context.WithoutCancel the three deletes below run on a context that
// is already done, every one of them fails, deleteJobDurability logs and
// returns, and the rows are stranded for the life of the database -- no crash
// required.
func TestRemoveJob_DisconnectAfterDispatcherRemoveStillClearsDurability(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d := dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		disconnectingStore{Store: store.New(repo.DB()), cancel: cancel},
		application.runner,
	)
	application.dispatcher = d
	application.pipeline.dispatcher = d
	application.runner.report = d

	j, _ := removeJobFixture(t, application, "disconnected")

	logs := &lockedBuffer{}
	application.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// All three seeded explicitly. removeJobFixture goes through
	// Dispatcher().Add rather than the ingest path, so it writes no job_files
	// row of its own -- the guard below is what caught that, and it stays
	// because a fixture that silently stops seeding turns this into a test
	// that asserts nothing (#547).
	if _, err := application.runs.Commit(ctx, j.ID(), []durability.DurableArticle{
		{FileIdx: 0, ArtIdx: 0, Offset: 0, Length: 100, CRC32: 1},
	}); err != nil {
		t.Fatalf("seed durable runs: %v", err)
	}
	if _, err := repo.DB().ExecContext(ctx,
		`INSERT INTO failed_articles (job_id, art_idx) VALUES (?, 1)`, j.ID()); err != nil {
		t.Fatalf("seed failed articles: %v", err)
	}
	if _, err := repo.DB().ExecContext(ctx,
		`INSERT INTO job_files (job_id, file_index, complete) VALUES (?, 0, 0)`, j.ID()); err != nil {
		t.Fatalf("seed job files: %v", err)
	}
	if nr, nf := durabilityRowCounts(t, application, j.ID()); nr != 1 || nf != 1 {
		t.Fatalf("fixture recorded %d runs and %d failed rows, want 1 and 1; "+
			"the test would pass vacuously", nr, nf)
	}
	if n := jobFilesCount(t, application, j.ID()); n == 0 {
		t.Fatal("no job_files rows to delete, so this test would pass vacuously")
	}

	if err := application.RemoveJob(ctx, j.ID(), false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the store never cancelled, so the removal never entered the window under test")
	}

	// The assembler's cancel is detached for the same reason and pinned here
	// rather than in its own test, because it is the same window: CancelJob
	// returns ctx.Err() before enqueueing anything when its context is already
	// done, so on a cancellable context the worker never closes this job's
	// handles and they leak for the life of the process.
	//
	// Asserted through the warning rather than through the handles themselves,
	// because internal/app has no view of them -- Assembler exports no
	// open-handle accessor, and syncTargetFor is nil by this point because it
	// resolves through app.dispatcher.Job, which dispatcher.Remove has already
	// made return false (durability.go). The warning is the only place
	// RemoveJob records whether the close was confirmed, which makes it the
	// observable the production code actually offers.
	if s := logs.String(); strings.Contains(s, "assembler cancel job did not confirm") {
		t.Errorf("RemoveJob reported unconfirmed file handles after a caller "+
			"disconnect; log was:\n%s", s)
	}

	if nr, nf := durabilityRowCounts(t, application, j.ID()); nr != 0 || nf != 0 {
		t.Errorf("%d durable runs and %d failed-article rows survive a removal whose "+
			"caller disconnected after the job left the dispatcher", nr, nf)
	}
	if n := jobFilesCount(t, application, j.ID()); n != 0 {
		t.Errorf("%d job_files rows survive a removal whose caller disconnected after "+
			"the job left the dispatcher", n)
	}
}

// removeJobFixture adds a two-article job whose single file the assembler is
// holding open, and returns the job with the resolved path of that file.
func removeJobFixture(t *testing.T, application *Application, name string) (*job.Job, string) {
	t.Helper()

	if err := application.assembler.Start(t.Context()); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	t.Cleanup(func() { _ = application.assembler.Stop() })

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: name + ".bin",
		Bytes:   200,
		Articles: []nzb.Article{
			{ID: name + "0@t", Bytes: 100, Number: 1},
			{ID: name + "1@t", Bytes: 100, Number: 2},
		},
	}}}
	j, hdr, err := BuildIngestJob(application.config, parsed, name+".nzb", types.FetchOptions{NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := application.Dispatcher().Add(j, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	writeFixtureArticle(t, application, j.ID(), 0, 0)
	info, err := application.pipeline.resolveFileInfo(j.ID(), 0)
	if err != nil {
		t.Fatalf("resolveFileInfo: %v", err)
	}
	return j, info.Path
}

// TestRemoveJob_KeepingFilesLeavesAFileTheAssemblerHoldsOpen pins #433.
func TestRemoveJob_KeepingFilesLeavesAFileTheAssemblerHoldsOpen(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	j, path := removeJobFixture(t, application, "kept")

	if err := application.RemoveJob(t.Context(), j.ID(), false); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the job's in-flight file is gone after RemoveJob(deleteFiles=false) (stat: %v)", err)
	}
}

// TestRemoveJob_DeletingFilesRemovesAFileTheAssemblerHoldsOpen pins the user-visible
// outcome of deleteFiles=true: the bytes are gone.
func TestRemoveJob_DeletingFilesRemovesAFileTheAssemblerHoldsOpen(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	j, path := removeJobFixture(t, application, "swept")

	if err := application.RemoveJob(t.Context(), j.ID(), true); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the job's in-flight file survives RemoveJob(deleteFiles=true) (stat err: %v, want IsNotExist)", err)
	}
}
