package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
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

	// Independent of the store under test: reconstructed from the admin dir
	// the test's config carries, the same way newLifecycleTestApp built the
	// path it actually called history.Open with. Reading wantPath back from
	// realStore(t, application).Path() instead would be circular — it is
	// the SUT's own output, so a wrong app.go computation would still match
	// itself.
	wantPath := filepath.Join(application.config.GetGeneral().AdminDir, "history.db")

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

// installFinalizeCommitWrap gives application a barrier over its real store
// whose commits fail with fail's error, and run normally when it returns nil.
func installFinalizeCommitWrap(t *testing.T, application *Application, fail func() error) {
	t.Helper()
	wrap := func(_ context.Context, _ string, commit func() ([]durability.Collision, error)) ([]durability.Collision, error) {
		if err := fail(); err != nil {
			return nil, err
		}
		return commit()
	}
	application.barrier = durability.NewBarrier(
		realStore(t, application),
		application, application, slog.New(slog.DiscardHandler),
		durability.WithCommitWrap(wrap),
	)
}

// TestHandleFileComplete_AFinalizeCommitErrorStallsNamingTheStore pins the
// finalize half of R27 for the durability record: a completed file whose
// commit fails stalls the job once, under a reason naming the database and the
// commit, and the finalize is recorded so a re-evaluation can retry it.
//
// The finalize's commit error used to reach routeFinalizeFailure unrouted,
// which classified it as a "finalize" fault on the completed file — a reason
// that sends the operator to a download disk that did not fail.
func TestHandleFileComplete_AFinalizeCommitErrorStallsNamingTheStore(t *testing.T) {
	t.Parallel()
	// The second of two files, so a retry recorded against the wrong file
	// cannot complete this one. Its one article has global index 1.
	const fileIdx, artIdx = 1, 1
	application, job := newDurabilityTestApp(t, 2, 1)
	writeFixtureArticle(t, application, job.ID(), fileIdx, artIdx)
	info, err := application.pipeline.resolveFileInfo(job.ID(), fileIdx)
	if err != nil || info.Path == "" {
		t.Fatalf("resolveFileInfo = %+v, %v; the test needs the file's path to tell it apart", info, err)
	}

	var failing atomic.Bool
	failing.Store(true)
	installFinalizeCommitWrap(t, application, func() error {
		if failing.Load() {
			return errors.New("database or disk is full")
		}
		return nil
	})
	// Independent of the store under test, as in the checkpoint test above.
	wantPath := filepath.Join(application.config.GetGeneral().AdminDir, "history.db")

	application.handleFileComplete(t.Context(), FileComplete{JobID: job.ID(), FileIdx: fileIdx})

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if row.Status() != constants.StatusPaused {
		t.Errorf("status = %v after a failed finalize commit, want Paused", row.Status())
	}
	reason := application.StallReason(job.ID()).Reason
	if !strings.Contains(reason, `commit "`+wantPath+`"`) {
		t.Errorf("stall reason = %q, want it to name the commit on %q (R27)", reason, wantPath)
	}
	if strings.Contains(reason, info.Path) {
		t.Errorf("stall reason = %q names the completed file %q; the file's disk did not fail",
			reason, info.Path)
	}
	if !application.hasPendingFinalize(job.ID(), fileIdx) {
		t.Fatal("the failed finalize was not recorded, so nothing retries it and the file " +
			"never completes")
	}
	if job.Progress().FileComplete(fileIdx) {
		t.Error("the file was marked complete although its finalize did not commit")
	}

	failing.Store(false)
	application.reevaluateStalls(t.Context())

	if !job.Progress().FileComplete(fileIdx) || !job.Progress().ArticleDone(artIdx) {
		t.Error("the retried finalize did not complete the file and ack its article")
	}
	if got := application.StallReason(job.ID()).Reason; got != "" {
		t.Errorf("stall reason = %q after the retry landed, want it cleared", got)
	}
}

// TestHandleFileComplete_AFinalizeCommitAbandonedByItsCallerIsNotStalled pins
// the caller-cancel carve-out on the finalize path: a commit that failed after
// the caller's context ended parks nothing, and the finalize is still owed.
func TestHandleFileComplete_AFinalizeCommitAbandonedByItsCallerIsNotStalled(t *testing.T) {
	t.Parallel()
	application, job := newDurabilityTestApp(t, 1, 1)
	writeFixtureArticle(t, application, job.ID(), 0, 0)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var failing atomic.Bool
	failing.Store(true)
	installFinalizeCommitWrap(t, application, func() error {
		if failing.Load() {
			// The driver's own error for an interrupted statement, which
			// wraps no context error: only the context says who ended it.
			cancel()
			return errors.New("interrupted")
		}
		return nil
	})

	application.handleFileComplete(ctx, FileComplete{JobID: job.ID(), FileIdx: 0})

	row, ok := application.dispatcher.Row(job.ID())
	if !ok {
		t.Fatal("job left the dispatcher")
	}
	if row.Status() == constants.StatusPaused {
		t.Error("a commit its caller abandoned paused the job, naming a disk that did not fail")
	}
	if got := application.StallReason(job.ID()).Reason; got != "" {
		t.Errorf("stall reason = %q for a caller's cancellation, want none", got)
	}
	if !application.hasPendingFinalize(job.ID(), 0) {
		t.Fatal("the abandoned finalize was not recorded for retry; the file never completes")
	}

	failing.Store(false)
	application.reevaluateStalls(t.Context())

	if !job.Progress().FileComplete(0) {
		t.Error("the retried finalize did not complete the file")
	}
}
