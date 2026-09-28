package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hobeone/gonzbd/internal/checkpoint"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

var errJobCheckpoint = errors.New("checkpoint write failed for the chosen job")

// failForJobStore fails every checkpoint batch that carries failID and passes
// the rest to the store production uses.
type failForJobStore struct {
	checkpoint.Store
	failID string
}

func (s failForJobStore) SaveBatch(ctx context.Context, cps []job.Checkpoint) error {
	for _, cp := range cps {
		if cp.ID == s.failID {
			return errJobCheckpoint
		}
	}
	return s.Store.SaveBatch(ctx, cps)
}

// TestRetryHistoryJob_FailedFlushLeavesNothingMarked: a retry whose own
// checkpoint write fails gives up the job it built, so no later flush writes
// that abandoned attempt's state over rows its reclaim took.
func TestRetryHistoryJob_FailedFlushLeavesNothingMarked(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)
	const id = "retryflushfails1"
	application.WrapCheckpointStore(func(s checkpoint.Store) checkpoint.Store {
		return failForJobStore{Store: s, failID: id}
	})

	writeGzNZB(t, adminDir, "flushfails.nzb.gz", retryNZBWithRecoveryVolume(2, 1))
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "flushfails",
		NzbName:   "flushfails.nzb",
		NZBBackup: "flushfails.nzb.gz",
		Status:    string(constants.StatusFailed),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}

	if err := application.RetryHistoryJob(t.Context(), id); !errors.Is(err, errJobCheckpoint) {
		t.Fatalf("RetryHistoryJob = %v, want the checkpoint write's error", err)
	}
	if got := application.Checkpointer().DirtyCount(); got != 0 {
		t.Fatalf("DirtyCount = %d after the retry gave up, want 0: the abandoned attempt is "+
			"still marked, and the next flush writes it", got)
	}
}

// TestRetryHistoryJob_AnotherJobsCheckpointFailureDoesNotFailTheRetry: the
// retry flushes its own job's checkpoint, so a store that cannot write some
// other queued job neither fails the retry nor stops the retried job's rows
// reaching disk before RetryHistoryJob returns.
func TestRetryHistoryJob_AnotherJobsCheckpointFailureDoesNotFailTheRetry(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newRetryTestApp(t)
	const unrelated = "unrelatedjob0001"
	application.WrapCheckpointStore(func(s checkpoint.Store) checkpoint.Store {
		return failForJobStore{Store: s, failID: unrelated}
	})
	application.Checkpointer().Mark(job.New(unrelated, "unrelated", job.PolicyFromPP(3)))

	writeGzNZB(t, adminDir, "isolated.nzb.gz", retryNZBWithRecoveryVolume(2, 1))
	const id = "retryisolated001"
	if err := repo.Add(t.Context(), history.Entry{
		NzoID:     id,
		Name:      "isolated",
		NzbName:   "isolated.nzb",
		NZBBackup: "isolated.nzb.gz",
		Status:    string(constants.StatusFailed),
	}, nil); err != nil {
		t.Fatalf("repo.Add: %v", err)
	}
	seedHistoryJobFilesRow(t, repo.DB(), id, 0, false, 2, job.FetchAlways)
	seedCompletedFile(t, repo.DB(), id, 1, 2, 1)

	if err := application.RetryHistoryJob(t.Context(), id); err != nil {
		t.Fatalf("RetryHistoryJob: %v — another job's checkpoint failure failed this retry", err)
	}

	// The seed writes complete = 0; only the retry's own checkpoint write
	// carries the retained complete = 1, so this reads that write.
	j, ok := application.Dispatcher().Job(id)
	if !ok {
		t.Fatal("retried job is not in the queue")
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	idx := recoveryFileIndex(t, m)
	var complete int
	if err := repo.DB().QueryRowContext(t.Context(),
		`SELECT complete FROM job_files WHERE job_id = ? AND file_index = ?`, id, idx).Scan(&complete); err != nil {
		t.Fatalf("read job_files: %v", err)
	}
	if complete != 1 {
		t.Errorf("job_files.complete = %d for file %d when RetryHistoryJob returned, want 1: "+
			"the retried job's checkpoint was not on disk, so an eviction re-hydrates the "+
			"file as incomplete", complete, idx)
	}
	if got := application.Checkpointer().DirtyCount(); got != 1 {
		t.Errorf("DirtyCount = %d, want 1: the unrelated job's mark should wait for the next flush", got)
	}
}
