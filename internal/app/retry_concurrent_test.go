package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
)

// blockingAdmitStore holds the first Admit until release is closed, reporting
// through entered that the caller is inside it. A retry seeds job_files
// through Admit before it registers with the dispatcher, so this holds it in
// the span where nothing but the transition lock keeps a second actor out.
// Only the first call blocks: an actor that wrongly gets past the lock runs to
// completion rather than hanging the test.
type blockingAdmitStore struct {
	durabilityStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *blockingAdmitStore) Admit(ctx context.Context, jobID string, fetch []uint8) error {
	first := false
	s.once.Do(func() { first = true; close(s.entered) })
	if first {
		<-s.release
	}
	return s.durabilityStore.Admit(ctx, jobID, fetch)
}

// blockRetryInAdmit swaps the application's durability store for one that
// holds the first Admit, and returns it.
func blockRetryInAdmit(application *Application) *blockingAdmitStore {
	blk := &blockingAdmitStore{
		durabilityStore: application.durable,
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	application.durable = blk
	return blk
}

// addRetryableEntry files a FAILED history entry for jobID with an NZB backup
// the retry can rebuild from, and returns the backup's path. path is the
// entry's download directory, or "" for none.
func addRetryableEntry(t *testing.T, repo *history.Repository, adminDir, jobID, path string) string {
	t.Helper()
	backup := jobID + ".nzb.gz"
	writeRetryNZBBackup(t, adminDir, backup, retryFixtureNZB(1))
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: jobID, Name: "retry-" + jobID, NzbName: jobID + ".nzb",
		NZBBackup: backup, Category: "*", Status: "Failed", Completed: time.Now(),
		Path: path,
	}); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	return filepath.Join(adminDir, "nzb", backup)
}

func manifestPathOf(t *testing.T, adminDir, jobID string) string {
	t.Helper()
	p, err := manifestPath(adminDir, jobID)
	if err != nil {
		t.Fatalf("manifestPath: %v", err)
	}
	return p
}

// TestRetryHistoryJob_RefusesWhileAnotherHolderHasTheID pins that while any
// actor holds a job ID's transition lock, a retry of it refuses before it
// touches anything. The holder here is not a retry, so a retry that claims
// some other key than its job's cannot pass by excluding only itself.
func TestRetryHistoryJob_RefusesWhileAnotherHolderHasTheID(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const jobID = "feedface0000a001"
	addRetryableEntry(t, repo, adminDir, jobID, "")
	seedDurability(t, application, jobID)

	claim, err := application.transitions.acquire(t.Context(), jobID)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer claim.release()

	err = application.RetryHistoryJob(t.Context(), jobID)
	if !errors.Is(err, errJobInTransition) {
		t.Fatalf("RetryHistoryJob err = %v, want errJobInTransition", err)
	}
	if written, files := durabilityRowCounts(t, application, jobID); written != 1 || files != 1 {
		t.Errorf("written_articles, job_files = %d, %d, want 1, 1: the refused retry reclaimed "+
			"state another actor holds", written, files)
	}
	if _, err := os.Stat(manifestPathOf(t, adminDir, jobID)); !os.IsNotExist(err) {
		t.Errorf("a refused retry wrote a queue manifest (stat err = %v)", err)
	}
}

// TestRetryHistoryJob_LosingConcurrentRetryIsRefusedBeforeActing is two
// retries of one job at once. The first is held after its entry and before it
// registers with the dispatcher, where only the transition lock excludes the
// second; the second must be refused, and the first must then run to a queued
// job with its rows and manifest intact.
func TestRetryHistoryJob_LosingConcurrentRetryIsRefusedBeforeActing(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const jobID = "feedface0000c0de"
	addRetryableEntry(t, repo, adminDir, jobID, "")
	blk := blockRetryInAdmit(application)

	winner := make(chan error, 1)
	go func() { winner <- application.RetryHistoryJob(context.Background(), jobID) }()
	select {
	case <-blk.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first retry never reached its job_files seed")
	}

	err := application.RetryHistoryJob(t.Context(), jobID)
	close(blk.release)
	if !errors.Is(err, errJobInTransition) {
		t.Errorf("second retry err = %v, want errJobInTransition", err)
	}
	if err := <-winner; err != nil {
		t.Fatalf("the first retry failed: %v", err)
	}
	if n := jobFilesCount(t, application, jobID); n == 0 {
		t.Error("the queued job has no job_files rows, so its progress goes unrecorded")
	}
	if _, err := os.Stat(manifestPathOf(t, adminDir, jobID)); err != nil {
		t.Errorf("the queued job has no manifest, so its first eviction cannot hydrate it: %v", err)
	}
}

// TestRetryHistoryJob_RefusesAJobTheDispatcherHolds: a FAILED entry can exist
// beside a queued job of the same ID (see errJobAlreadyQueued). A retry of it
// must refuse before acting on the queued job's state. With
// no retained progress filed, the retry would otherwise drop its durable runs.
func TestRetryHistoryJob_RefusesAJobTheDispatcherHolds(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const jobID = "feedface0000a002"
	addRetryableEntry(t, repo, adminDir, jobID, "")
	entry, err := repo.Get(t.Context(), jobID)
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	j, hdr, err := application.rebuildJobFromNZB(*entry)
	if err != nil {
		t.Fatalf("rebuildJobFromNZB: %v", err)
	}
	if err := application.dispatcher.Add(t.Context(), j, hdr); err != nil {
		t.Fatalf("dispatcher.Add: %v", err)
	}
	if err := writeJobManifest(adminDir, j); err != nil {
		t.Fatalf("writeJobManifest: %v", err)
	}
	seedDurability(t, application, jobID)
	manifestBefore, err := os.ReadFile(manifestPathOf(t, adminDir, jobID))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	err = application.RetryHistoryJob(t.Context(), jobID)
	if !errors.Is(err, errJobAlreadyQueued) {
		t.Fatalf("RetryHistoryJob err = %v, want errJobAlreadyQueued", err)
	}
	if runs, _ := durabilityRowCounts(t, application, jobID); runs != 1 {
		t.Errorf("durable runs = %d, want 1: the retry discarded a queued job's runs", runs)
	}
	manifestAfter, err := os.ReadFile(manifestPathOf(t, adminDir, jobID))
	if err != nil {
		t.Fatalf("the queued job's manifest is gone: %v", err)
	}
	if !bytes.Equal(manifestBefore, manifestAfter) {
		t.Error("the retry rewrote a queued job's manifest")
	}
}
