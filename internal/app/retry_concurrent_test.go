package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/dispatch/store"
	"github.com/hobeone/gonzbd/internal/history"
)

// blockingSaveStore holds the first Save until release is closed, reporting
// through entered that the caller is inside it, which holds a job in the
// window between Dispatcher.Add registering it and its first queue row
// reaching dispatch_jobs.
type blockingSaveStore struct {
	dispatch.Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *blockingSaveStore) Save(ctx context.Context, p dispatch.Persisted) error {
	first := false
	s.once.Do(func() { first = true; close(s.entered) })
	if first {
		<-s.release
	}
	return s.Store.Save(ctx, p)
}

// TestRetryHistoryJob_LosingConcurrentRetryKeepsTheWinnersRows pins the
// cleanup of a retry that loses to a concurrent retry of the same job.
//
// The winner registers with the dispatcher before its first queue row is
// written. A second retry that reaches dispatcher.Add inside that window fails
// on the duplicate ID. Its abandon-cleanup must leave the job's rows alone:
// the reclaim rule reads only persisted dispatch_jobs, so it would see no
// queue row and delete job_files the winner is about to run with.
func TestRetryHistoryJob_LosingConcurrentRetryKeepsTheWinnersRows(t *testing.T) {
	application, repo, adminDir := newLifecycleTestApp(t)
	blk := &blockingSaveStore{
		Store:   store.New(repo.DB()),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	application.dispatcher = dispatch.New(
		1, 1, time.Second, time.Now,
		&appWorkers{app: application},
		application.residency,
		blk,
		application.runner,
	)

	const jobID = "feedface0000c0de"
	const nzbBackup = "retry-concurrent.nzb.gz"
	writeRetryNZBBackup(t, adminDir, nzbBackup, retryFixtureNZB(1))
	if err := repo.Add(t.Context(), history.Entry{
		NzoID: jobID, Name: "retry-concurrent", NzbName: "retry-concurrent.nzb",
		NZBBackup: nzbBackup, Category: "*", Status: "Failed", Completed: time.Now(),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	winner := make(chan error, 1)
	go func() { winner <- application.RetryHistoryJob(context.Background(), jobID) }()
	select {
	case <-blk.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first retry never reached its first queue-row write")
	}

	if err := application.RetryHistoryJob(t.Context(), jobID); err == nil {
		t.Fatal("a second retry of a job the dispatcher already holds succeeded")
	}
	close(blk.release)
	if err := <-winner; err != nil {
		t.Fatalf("the first retry failed: %v", err)
	}

	var n int
	if err := repo.DB().QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM job_files WHERE job_id = ?", jobID).Scan(&n); err != nil {
		t.Fatalf("count job_files: %v", err)
	}
	if n == 0 {
		t.Error("the queued job has no job_files rows: the losing retry's cleanup reclaimed " +
			"them while the winner was registered but not yet persisted, so its progress " +
			"goes unrecorded and an eviction hydrates it without its fetch policy")
	}
}
