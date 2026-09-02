package queue_test

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// The par2 release reason has to outlive the process that computed it.
//
// It was process-local while its only consumer ran in the same process as the
// verdict. It is not once the post-processing file list branches on it: a
// restart between the verdict and post-processing would leave the reason empty
// and the summary would print "verified clean from index" for a job nothing
// verified.

func par2ReasonJob(t *testing.T, q *queue.Queue, name string) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{Subject: "payload", Articles: []nzb.Article{{ID: "a1", Number: 1}}, Bytes: 100},
		},
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: name}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// A resident status is required, not cosmetic: SQLiteStore.Get builds
	// job.progress only on its resident branch, so a job left at NewJob's
	// default StatusQueued reloads with a nil progress and reports an empty
	// reason through the accessor's nil guard — which would pass this test
	// for the wrong reason.
	if err := q.SetStatus(job.ID, constants.StatusDownloading); err != nil {
		t.Fatalf("SetStatus(Downloading): %v", err)
	}
	return job
}

func TestSQLiteStore_Par2ReleaseReasonSurvivesAReload(t *testing.T) {
	t.Parallel()
	store, _, dir := setupTestStore(t)

	q := queue.New(queue.WithStore(store), queue.WithStateDir(dir))
	job := par2ReasonJob(t, q, "resident-reason")

	const reason = "no delivered file matches any par2 entry"
	if err := q.SetPar2ReleaseReason(job.ID, reason); err != nil {
		t.Fatalf("SetPar2ReleaseReason: %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loadedQ, err := queue.Load(dir, queue.WithStore(store))
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}
	loaded := loadedQ.SnapshotJob(job.ID)
	if loaded == nil {
		t.Fatal("job missing from queue after reload")
	}
	if got := loaded.Progress().Par2ReleaseReason(); got != reason {
		t.Errorf("reloaded Par2ReleaseReason = %q, want %q", got, reason)
	}
}

func TestSQLiteStore_NonResidentJobKeepsItsPar2ReleaseReason(t *testing.T) {
	t.Parallel()
	store, _, dir := setupTestStore(t)

	q := queue.New(queue.WithStore(store), queue.WithStateDir(dir))
	job := par2ReasonJob(t, q, "paused-reason")

	const reason = "no delivered file matches any par2 entry"
	if err := q.SetPar2ReleaseReason(job.ID, reason); err != nil {
		t.Fatalf("SetPar2ReleaseReason: %v", err)
	}
	if err := q.SetStatus(job.ID, constants.StatusPaused); err != nil {
		t.Fatalf("SetStatus(Paused): %v", err)
	}
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loadedQ, err := queue.Load(dir, queue.WithStore(store))
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}
	if got := loadedQ.SnapshotJob(job.ID).Progress().Par2ReleaseReason(); got != reason {
		t.Errorf("reloaded Par2ReleaseReason = %q, want %q", got, reason)
	}

	// The destructive half, and the reason this test exists separately from
	// the resident one. A non-resident job is hydrated with a fresh progress
	// that has no reason, so once updateTx writes the column, the next update
	// on that job encodes the empty string back over the stored value and the
	// reason is gone for good. The download stamps hit exactly this and
	// persistence.go records it verbatim.
	if err := loadedQ.SetPriority(job.ID, constants.HighPriority); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	reloadedQ, err := queue.Load(dir, queue.WithStore(store))
	if err != nil {
		t.Fatalf("second queue.Load: %v", err)
	}
	if got := reloadedQ.SnapshotJob(job.ID).Progress().Par2ReleaseReason(); got != reason {
		t.Errorf("Par2ReleaseReason after an update on the reloaded job = %q, want %q — the update erased it", got, reason)
	}
}
