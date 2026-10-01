package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/durability"
)

// TestCheckpointJob_ACommitErrorStallsTheJobUntilReevaluated pins the whole
// route of a failed durability-record commit: the barrier routes it to
// Application.Stall, which pauses the job with a surfaced reason, and the
// stall re-evaluation resumes the job once it runs.
//
// The commit error is a plain one, as SQLite's are. Before the barrier routed
// it, checkpointJob logged a Warn and returned, and the job went on
// downloading bytes that no barrier could record.
func TestCheckpointJob_ACommitErrorStallsTheJobUntilReevaluated(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 2)
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	var failing atomic.Bool
	failing.Store(true)
	wrap := func(_ context.Context, _ string, commit func() ([]durability.Collision, error)) ([]durability.Collision, error) {
		if failing.Load() {
			return nil, errors.New("database or disk is full")
		}
		return commit()
	}
	application.barrier = durability.NewBarrier(
		realStore(t, application),
		application, application, slog.New(slog.DiscardHandler),
		durability.WithCommitWrap(wrap),
	)

	wantPath := realStore(t, application).Path()
	if wantPath == "" {
		t.Fatal("the application's real store carries no path to assert against")
	}

	application.checkpointJob(t.Context(), job.ID())

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v after a failed commit, want Paused — the job keeps "+
			"downloading bytes no barrier can record", row.Status())
	}
	reason := application.StallReason(job.ID()).Reason
	if !strings.Contains(reason, "commit") || !strings.Contains(reason, "database or disk is full") {
		t.Errorf("stall reason = %q, want it to name the commit and the store's error (R27)", reason)
	}
	if !strings.Contains(reason, wantPath) {
		t.Errorf("stall reason = %q, want it to name the store's path %q (R27) — "+
			"the barrier learned nothing about the database's file from the commit "+
			"itself and has no other way to find it", reason, wantPath)
	}
	if application.hasBarrierStamp(job.ID()) {
		t.Error("a barrier whose commit failed stamped last_barrier")
	}

	failing.Store(false)
	application.reevaluateStalls(t.Context())

	row, ok = application.dispatcher.Row(job.ID())
	if !ok || row.Status() == constants.StatusPaused {
		t.Errorf("status = %v after re-evaluation, want the job off Paused — a stall "+
			"that is never re-evaluated needs a restart to clear (R19)", row.Status())
	}
	if got := application.StallReason(job.ID()).Reason; got != "" {
		t.Errorf("stall reason = %q after the job resumed, want it cleared", got)
	}

	// The drain report the failed commit left unconfirmed is re-reported, so
	// the next barrier records and acks the article the failure did not.
	application.checkpointJob(t.Context(), job.ID())

	if !application.hasBarrierStamp(job.ID()) {
		t.Error("the barrier after the resume did not commit")
	}
	if n, err := job.CountUnfinishedArticles(0); err != nil || n != 1 {
		t.Errorf("unfinished = %d (err %v), want 1 — the article written before the "+
			"failed commit must be acked by the next one", n, err)
	}
	if got := application.StallReason(job.ID()).Reason; got != "" {
		t.Errorf("stall reason = %q, want none once the commit succeeds", got)
	}
}
