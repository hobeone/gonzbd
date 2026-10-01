package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/checkpoint"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
)

// staleMarkJob builds a one-file, two-article job under id.
func staleMarkJob(t *testing.T, id string) *job.Job {
	t.Helper()
	j := job.New(id, id, job.PolicyFromPP(3))
	m := job.NewManifest([]job.JobFile{{
		Subject: "f.bin",
		Bytes:   200,
		Articles: []job.JobArticle{
			{ID: "a1@example.com", Bytes: 100, Number: 1},
			{ID: "a2@example.com", Bytes: 100, Number: 2},
		},
	}})
	if err := j.AttachContent(m); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	return j
}

// TestCheckpoint_StaleMarkAfterPruneDoesNotReachARetry drives, against the real
// SQLite store, a result for a departed job instance that marks it after its
// departure's Prune, followed by a retry of the same ID seeding job_files and a
// periodic flush landing before the retry's own mark.
//
// SaveProgress gates the failed_articles insert on the job having job_files,
// and the retry's seed satisfies that gate for the departed instance too, so
// the only thing standing between the stale mark and the retried job is the
// checkpointer refusing a mark of an instance it has pruned. The retry's own
// flush only inserts, so a row written here would survive it and read back as
// a permanent failure the retry never re-attempts.
func TestCheckpoint_StaleMarkAfterPruneDoesNotReachARetry(t *testing.T) {
	ctx := t.Context()
	hdb, err := history.Open(ctx, filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = hdb.Close() })
	db := history.NewRepository(hdb).DB()
	st := durability.NewStore(db, "history.db")
	c := checkpoint.New(&appCheckpointStore{store: st}, time.Hour, nil)

	const id = "stale-job"
	// The first run: admitted, one article permanently failed, checkpointed.
	if err := st.Admit(ctx, id, []uint8{0}); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	old := staleMarkJob(t, id)
	if err := old.MarkArticleFailed(0); err != nil {
		t.Fatalf("MarkArticleFailed: %v", err)
	}
	c.Mark(old)
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("first run's Flush: %v", err)
	}

	// Its departure: Prune, then the reclaim, as both departure paths order it.
	c.Prune(old)
	if err := st.Reclaim(ctx, id); err != nil {
		t.Fatalf("departure Reclaim: %v", err)
	}

	// A late result for the departed instance: another permanent failure,
	// looked up before the departure and marked after its Prune returned.
	if err := old.MarkArticleFailed(1); err != nil {
		t.Fatalf("late MarkArticleFailed: %v", err)
	}
	c.Mark(old)

	// The retry, as RetryHistoryJob orders it: reclaim, then seed job_files.
	if err := st.Reclaim(ctx, id); err != nil {
		t.Fatalf("retry Reclaim: %v", err)
	}
	if err := st.Admit(ctx, id, []uint8{0}); err != nil {
		t.Fatalf("retry Admit: %v", err)
	}
	// The periodic flush lands between the seed and the retry's own mark.
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("periodic Flush: %v", err)
	}
	// The retry's own mark and flush.
	c.Mark(staleMarkJob(t, id))
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("retry Flush: %v", err)
	}

	failed, err := st.FailedArticles(context.WithoutCancel(ctx), id)
	if err != nil {
		t.Fatalf("FailedArticles: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("the retried job inherited failed articles %v from the departed instance; "+
			"a mark after Prune reached the store once the retry re-seeded job_files, "+
			"and the retry will never re-attempt them", failed)
	}
}

// TestRemoveJob_FailedRemovalKeepsCheckpointingTheJob: a RemoveJob whose
// dispatcher.Remove fails leaves the job registered, so the prune it made on
// the way must not outlive the call — the job's later marks are live ones, and
// refusing them would leave its job_files behind its progress for as long as
// it stays queued.
func TestRemoveJob_FailedRemovalKeepsCheckpointingTheJob(t *testing.T) {
	t.Parallel()
	application, j, _ := newAppWithCustomDispatchStore(t, 1)
	application.ctx = t.Context()

	if err := application.RemoveJob(t.Context(), j.ID(), false); err == nil {
		t.Fatal("RemoveJob succeeded; the store was set to fail its first delete")
	}
	if _, held := application.dispatcher.Job(j.ID()); !held {
		t.Fatal("the failed removal took the job out of the queue")
	}

	before := application.checkpointer.DirtyCount()
	application.checkpointer.Mark(j)
	if got := application.checkpointer.DirtyCount(); got != before+1 {
		t.Fatalf("DirtyCount after marking the still-registered job = %d, want %d; "+
			"the failed removal's prune still refuses its marks", got, before+1)
	}
}
