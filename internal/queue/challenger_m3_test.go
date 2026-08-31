package queue

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
)

func setupChallengerTestStore(t *testing.T) (*SQLiteStore, *history.Repository, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "history.db")
	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	return store, repo, dir
}

func makeChallengerTestJob(t *testing.T, id, name string) *Job {
	t.Helper()
	parsed := &nzb.NZB{
		Files: []nzb.File{
			{
				Subject:  name + ".bin",
				Bytes:    500,
				Articles: []nzb.Article{{ID: id + "@art1", Bytes: 250, Number: 1}, {ID: id + "@art2", Bytes: 250, Number: 2}},
			},
		},
	}
	job, err := NewJob(parsed, AddOptions{Name: name, Filename: name + ".nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	job.ID = id
	return job
}

// TestChallenger_M3_SQLiteTimestampPersistenceAndDaemonRestart verifies:
//  1. download_started and download_finished timestamps are correctly persisted in SQLiteStore.
//  2. Process restart / daemon restart (re-loading Queue + SQLiteStore from disk/db) recovers
//     exact timestamps and PostProc status.
func TestChallenger_M3_SQLiteTimestampPersistenceAndDaemonRestart(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "queue")
	dbPath := filepath.Join(dir, "history.db")

	db, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), stateDir, repo)
	q := New(WithStore(store), WithStateDir(stateDir))

	job := makeChallengerTestJob(t, "m3-crash-job-001", "crash-test-job")
	if err := q.Add(job); err != nil {
		t.Fatalf("q.Add: %v", err)
	}

	t1 := time.Now().Add(-15 * time.Minute).Truncate(time.Second).UTC()
	if err := q.MarkJobStarted(job.ID, t1); err != nil {
		t.Fatalf("MarkJobStarted: %v", err)
	}

	// Mark articles done so job completes
	ackDone(t, q, job.ID, "m3-crash-job-001@art1", "m3-crash-job-001@art2")
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	// Transition to post-processing (sets downloadFinished and PostProc = true)
	t2Pre := time.Now().Truncate(time.Second).UTC()
	started, err := q.SetPostProcStarted(job.ID)
	if err != nil || !started {
		t.Fatalf("SetPostProcStarted: started=%v err=%v", started, err)
	}
	t2Post := time.Now().Add(time.Second).Truncate(time.Second).UTC()

	snapPre := q.SnapshotJob(job.ID)
	if snapPre == nil {
		t.Fatal("SnapshotJob returned nil")
	}
	if !snapPre.PostProc {
		t.Error("expected snapPre.PostProc == true")
	}
	if snapPre.Progress().DownloadStarted().Unix() != t1.Unix() {
		t.Errorf("snapPre DownloadStarted = %v, want %v", snapPre.Progress().DownloadStarted(), t1)
	}
	dlFinishedPre := snapPre.Progress().DownloadFinished()
	if dlFinishedPre.IsZero() {
		t.Error("snapPre DownloadFinished should not be zero")
	}
	if dlFinishedPre.Before(t2Pre) || dlFinishedPre.After(t2Post) {
		t.Errorf("snapPre DownloadFinished %v out of range [%v, %v]", dlFinishedPre, t2Pre, t2Post)
	}

	// Save queue state to simulate clean or checkpointed shutdown
	if err := q.Save(stateDir); err != nil {
		t.Fatalf("q.Save: %v", err)
	}

	// --- SIMULATE DAEMON CRASH / RESTART ---
	_ = db.Close()

	// Re-open SQLite DB and re-instantiate Queue / Store
	db2, err := history.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("history.Open (restart): %v", err)
	}
	defer func() { _ = db2.Close() }()

	repo2 := history.NewRepository(db2)
	store2 := NewSQLiteStore(repo2.DB(), stateDir, repo2)

	// Verify store2.Get directly retrieves timestamps from SQLite
	gotStore, err := store2.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("store2.Get: %v", err)
	}
	if gotStore.Progress() == nil {
		t.Fatal("store2.Get returned nil progress")
	}
	if gotStore.Progress().DownloadStarted().Unix() != t1.Unix() {
		t.Errorf("store2.Get DownloadStarted = %v, want %v", gotStore.Progress().DownloadStarted(), t1)
	}
	if gotStore.Progress().DownloadFinished().Unix() != dlFinishedPre.Unix() {
		t.Errorf("store2.Get DownloadFinished = %v, want %v", gotStore.Progress().DownloadFinished(), dlFinishedPre)
	}

	// Reload full queue from disk & store
	q2, err := Load(stateDir, WithStore(store2))
	if err != nil {
		t.Fatalf("queue.Load: %v", err)
	}

	reloadedSnap := q2.SnapshotJob(job.ID)
	if reloadedSnap == nil {
		t.Fatalf("job %s missing after queue restart", job.ID)
	}
	if !reloadedSnap.PostProc {
		t.Error("expected reloadedSnap.PostProc == true after restart")
	}
	if reloadedSnap.Progress().DownloadStarted().Unix() != t1.Unix() {
		t.Errorf("reloaded DownloadStarted = %v, want %v", reloadedSnap.Progress().DownloadStarted(), t1)
	}
	if reloadedSnap.Progress().DownloadFinished().Unix() != dlFinishedPre.Unix() {
		t.Errorf("reloaded DownloadFinished = %v, want %v", reloadedSnap.Progress().DownloadFinished(), dlFinishedPre)
	}
}

// TestChallenger_M3_ConcurrentQueueMutationAndSnapshotRaces stress tests concurrent calls:
// - SetPostProcStarted
// - MarkDownloadFinished
// - MarkJobStarted
// - SnapshotJob / Snapshot
// - Progress readers
// Running under go test -race ensures zero data races exist across these entry points.
func TestChallenger_M3_ConcurrentQueueMutationAndSnapshotRaces(t *testing.T) {
	store, _, dir := setupChallengerTestStore(t)
	q := New(WithStore(store), WithStateDir(dir))

	const numJobs = 20
	jobIDs := make([]string, numJobs)
	for i := range numJobs {
		id := fmt.Sprintf("race-job-%03d", i)
		jobIDs[i] = id
		job := makeChallengerTestJob(t, id, fmt.Sprintf("race-name-%d", i))
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	workers := 20
	for w := range workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(workerID), uint64(time.Now().UnixNano())))
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				id := jobIDs[r.IntN(numJobs)]
				now := time.Now().UTC()

				switch r.IntN(6) {
				case 0:
					_, _ = q.SetPostProcStarted(id)
				case 1:
					_ = q.MarkDownloadFinished(id, now)
				case 2:
					_ = q.MarkJobStarted(id, now)
				case 3:
					snap := q.SnapshotJob(id)
					if snap != nil && snap.Progress() != nil {
						_ = snap.Progress().DownloadStarted()
						_ = snap.Progress().DownloadFinished()
						_ = snap.IsComplete()
					}
				case 4:
					snaps := q.Snapshot()
					for _, s := range snaps {
						if s != nil && s.Progress() != nil {
							_ = s.Progress().DownloadStarted()
						}
					}
				case 5:
					_ = q.SetStatus(id, constants.StatusDownloading)
				}
			}
		}(w)
	}

	wg.Wait()
}

// TestChallenger_M3_SnapshotMutationIsolation asserts that mutating a SnapshotJob object
// does NOT mutate the live Queue object.
func TestChallenger_M3_SnapshotMutationIsolation(t *testing.T) {
	q := New()
	job := makeChallengerTestJob(t, "iso-job-001", "iso-test")
	if err := q.Add(job); err != nil {
		t.Fatalf("q.Add: %v", err)
	}

	t1 := time.Now().Add(-10 * time.Minute).Truncate(time.Second).UTC()
	_ = q.MarkJobStarted(job.ID, t1)
	_, _ = q.SetPostProcStarted(job.ID)

	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatal("SnapshotJob returned nil")
	}

	// Mutate the snapshot clone directly (e.g. legacy/buggy code trying to
	// alter snapshot). The progress arm writes the field rather than going
	// through a setter: what is under test is whether cloneJob deep-copied
	// JobProgress, and a setter would put its own guard between the test and
	// the property. This test used to call the exported
	// Job.MarkDownloadFinished, whose deletion in #457 is what surfaced that
	// — under a first-wins guard the write would have been refused, the
	// assertion below would still have passed, and it would have proved
	// nothing.
	origFinished := snap.Progress().DownloadFinished()
	if origFinished.IsZero() {
		t.Fatal("SetPostProcStarted did not set DownloadFinished; the mutation below would write into an empty field and prove nothing")
	}
	mutatedTime := time.Now().Add(5 * time.Hour).Truncate(time.Second).UTC()
	snap.progress.downloadFinished = mutatedTime
	snap.PostProc = false
	snap.Status = constants.StatusFailed

	// Fetch fresh snapshot from Queue
	freshSnap := q.SnapshotJob(job.ID)
	if freshSnap.PostProc != true {
		t.Errorf("Live queue PostProc was mutated by snapshot edit! got %v, want true", freshSnap.PostProc)
	}
	if freshSnap.Status != constants.StatusVerifying {
		t.Errorf("Live queue Status was mutated by snapshot edit! got %v, want %v", freshSnap.Status, constants.StatusVerifying)
	}
	// Equality against the value read BEFORE the mutation, not merely
	// inequality against mutatedTime: the negative form passes for any value
	// the live job might hold, including one no writer produced.
	if got := freshSnap.Progress().DownloadFinished(); !got.Equal(origFinished) {
		t.Errorf("Live queue DownloadFinished was mutated by snapshot edit: got %v, want %v", got, origFinished)
	}
}
