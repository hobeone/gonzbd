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

var errUnrelatedCheckpoint = errors.New("unrelated job's checkpoint write failed")

// failForJobStore fails every checkpoint batch that carries failID and passes
// the rest to the store production uses.
type failForJobStore struct {
	checkpoint.Store
	failID string
}

func (s failForJobStore) SaveBatch(ctx context.Context, cps []job.Checkpoint) error {
	for _, cp := range cps {
		if cp.ID == s.failID {
			return errUnrelatedCheckpoint
		}
	}
	return s.Store.SaveBatch(ctx, cps)
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
	seedHistoryJobFilesRow(t, repo.DB(), id, 1, true, 1, job.FetchAlways)

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
