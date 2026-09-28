package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/history"
)

// requireStillWaiting fails the test if done delivers within window: the call
// it reports on was expected to be held by another actor's transition claim.
func requireStillWaiting(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s returned (err = %v) while another actor held the job", what, err)
	case <-time.After(150 * time.Millisecond):
	}
}

// receiveWithin returns what done delivers, failing the test if nothing does
// within a generous bound.
func receiveWithin(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never returned after the holder released", what)
		return nil
	}
}

// startBlockedRetry starts a retry of jobID held in its job_files seed, after
// it has claimed the job, and returns its result channel and the blocker.
func startBlockedRetry(t *testing.T, application *Application, jobID string) (<-chan error, *blockingAdmitStore) {
	t.Helper()
	blk := blockRetryInAdmit(application)
	// A test that fails before releasing it would otherwise leave the retry
	// blocked for the rest of the run.
	t.Cleanup(func() {
		select {
		case <-blk.release:
		default:
			close(blk.release)
		}
	})
	retry := make(chan error, 1)
	go func() { retry <- application.RetryHistoryJob(context.Background(), jobID) }()
	select {
	case <-blk.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the retry never reached its job_files seed")
	}
	return retry, blk
}

// TestRemoveHistoryJob_ActsOnAFreshReadAfterAnInFlightRetry: a history removal
// waits out a retry of the same job, and then acts on what the history holds
// by then — nothing, since the retry requeued the job and deleted the entry.
// It must not have deleted the download directory or the NZB backup the
// requeued job now owns.
func TestRemoveHistoryJob_ActsOnAFreshReadAfterAnInFlightRetry(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const jobID = "feedface0000b001"
	dlDir := filepath.Join(application.config.GetGeneral().DownloadDir, "retry-"+jobID)
	if err := os.MkdirAll(dlDir, 0o750); err != nil {
		t.Fatalf("mkdir download dir: %v", err)
	}
	backup := addRetryableEntry(t, repo, adminDir, jobID, dlDir)
	retry, blk := startBlockedRetry(t, application, jobID)

	removed := make(chan error, 1)
	go func() { removed <- application.RemoveHistoryJob(context.Background(), jobID, true) }()
	requireStillWaiting(t, removed, "RemoveHistoryJob")
	close(blk.release)

	if err := receiveWithin(t, retry, "the retry"); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if err := receiveWithin(t, removed, "RemoveHistoryJob"); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("RemoveHistoryJob err = %v, want history.ErrNotFound: the entry was gone by the time it could act", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("the requeued job's NZB backup is gone: %v", err)
	}
	if _, err := os.Stat(dlDir); err != nil {
		t.Errorf("the requeued job's download directory is gone: %v", err)
	}
}

// TestMarkHistoryCompleted_WaitsForAnInFlightRetry: marking an entry completed
// waits out a retry of the same job, and then finds the entry gone.
func TestMarkHistoryCompleted_WaitsForAnInFlightRetry(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const jobID = "feedface0000b002"
	addRetryableEntry(t, repo, adminDir, jobID, "")
	retry, blk := startBlockedRetry(t, application, jobID)

	marked := make(chan error, 1)
	go func() { marked <- application.MarkHistoryCompleted(context.Background(), jobID) }()
	requireStillWaiting(t, marked, "MarkHistoryCompleted")
	close(blk.release)

	if err := receiveWithin(t, retry, "the retry"); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if err := receiveWithin(t, marked, "MarkHistoryCompleted"); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("MarkHistoryCompleted err = %v, want history.ErrNotFound", err)
	}
	if n := jobFilesCount(t, application, jobID); n == 0 {
		t.Error("the requeued job has no job_files rows")
	}
}

// TestDeleteHistoryEntries_RefusesAnIDOutsideItsClaim pins that the choke point
// for history deletion acts only on entries whose IDs its caller holds, and
// refuses the batch before touching any of it.
func TestDeleteHistoryEntries_RefusesAnIDOutsideItsClaim(t *testing.T) {
	t.Parallel()
	application, repo, adminDir := newLifecycleTestApp(t)
	const jobID = "feedface0000b003"
	backup := addRetryableEntry(t, repo, adminDir, jobID, "")
	entry, err := repo.Get(t.Context(), jobID)
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}

	claim := application.transitions.tryAcquire("some-other-job")
	defer claim.release()
	if _, err := application.deleteHistoryEntries(t.Context(), claim, []history.Entry{*entry}); err == nil {
		t.Fatal("deleteHistoryEntries deleted an entry its claim does not hold")
	}
	if _, err := repo.Get(t.Context(), jobID); err != nil {
		t.Errorf("the entry is gone: %v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("the backup is gone: %v", err)
	}
}

// TestWaitingPaths_GiveUpWithTheCallersContext: a removal or history change
// whose caller goes away while another actor holds the job returns the
// context's error and leaves the job's entry where it was.
func TestWaitingPaths_GiveUpWithTheCallersContext(t *testing.T) {
	t.Parallel()
	calls := map[string]func(*Application, context.Context, string) error{
		"RemoveJob": func(a *Application, ctx context.Context, id string) error {
			return a.RemoveJob(ctx, id, true)
		},
		"RemoveHistoryJob": func(a *Application, ctx context.Context, id string) error {
			return a.RemoveHistoryJob(ctx, id, true)
		},
		"MarkHistoryCompleted": func(a *Application, ctx context.Context, id string) error {
			return a.MarkHistoryCompleted(ctx, id)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			application, repo, adminDir := newLifecycleTestApp(t)
			const jobID = "feedface0000e001"
			addRetryableEntry(t, repo, adminDir, jobID, "")
			claim := application.transitions.tryAcquire(jobID)
			defer claim.release()

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := call(application, ctx, jobID); !errors.Is(err, context.Canceled) {
				t.Errorf("%s err = %v, want context.Canceled", name, err)
			}
			entry, err := repo.Get(t.Context(), jobID)
			if err != nil {
				t.Fatalf("the entry is gone: %v", err)
			}
			if entry.Status != "Failed" {
				t.Errorf("entry status = %q, want Failed", entry.Status)
			}
		})
	}
}

// TestStillExpired_KeepsOnlyHeldEntriesUnchangedSinceTheScan: given the
// retention scan's entries, the sweep keeps an entry only if its claim holds
// it and it is still filed with the Completed the scan read.
func TestStillExpired_KeepsOnlyHeldEntriesUnchangedSinceTheScan(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	old := time.Now().AddDate(0, 0, -90).Truncate(time.Second)
	const same, refiled, gone, unheld = "feedface0000d001", "feedface0000d002", "feedface0000d003", "feedface0000d004"
	for _, id := range []string{same, refiled, unheld} {
		completed := old
		if id == refiled {
			completed = time.Now().Truncate(time.Second)
		}
		if err := repo.Add(t.Context(), history.Entry{NzoID: id, Name: id, Status: "Failed", Completed: completed}, nil); err != nil {
			t.Fatalf("repo.Add: %v", err)
		}
	}
	scanned := make([]history.Entry, 0, 4)
	for _, id := range []string{same, refiled, gone, unheld} {
		scanned = append(scanned, history.Entry{NzoID: id, Completed: old})
	}

	claim := application.transitions.tryAcquire(same, refiled, gone)
	defer claim.release()
	got, err := application.stillExpired(t.Context(), claim, scanned)
	if err != nil {
		t.Fatalf("stillExpired: %v", err)
	}
	if len(got) != 1 || got[0].NzoID != same {
		ids := make([]string, 0, len(got))
		for _, e := range got {
			ids = append(ids, e.NzoID)
		}
		t.Errorf("stillExpired kept %v, want only %s: a re-filed, removed or unheld entry "+
			"must not reach the delete", ids, same)
	}
}

// TestStillExpired_ReportsAFailedReRead: a re-read that fails for any reason
// but the entry being gone fails the sweep rather than deleting on the scan's
// stale copy.
func TestStillExpired_ReportsAFailedReRead(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	const id = "feedface0000d005"
	if err := repo.DB().Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	claim := application.transitions.tryAcquire(id)
	defer claim.release()
	got, err := application.stillExpired(t.Context(), claim, []history.Entry{{NzoID: id}})
	if err == nil {
		t.Errorf("stillExpired = %v, nil on a database it cannot read", got)
	}
}

// TestPruneHistory_SkipsAJobInTransition pins that a retention sweep takes the
// expired entries no actor holds, leaves a held one for a later sweep, and
// takes it once it is free.
func TestPruneHistory_SkipsAJobInTransition(t *testing.T) {
	t.Parallel()
	application, repo, _ := newLifecycleTestApp(t)
	application.config.General.HistoryFailedRetentionDays = 30
	const held, free = "feedface0000b004", "feedface0000b005"
	for _, id := range []string{held, free} {
		if err := repo.Add(t.Context(), history.Entry{
			NzoID: id, Name: id, Status: "Failed", Completed: time.Now().AddDate(0, 0, -90),
		}, nil); err != nil {
			t.Fatalf("repo.Add: %v", err)
		}
	}

	claim := application.transitions.tryAcquire(held)
	if _, err := application.PruneHistory(t.Context()); err != nil {
		t.Fatalf("PruneHistory: %v", err)
	}
	if _, err := repo.Get(t.Context(), free); err == nil {
		t.Error("the sweep left an expired entry no actor holds")
	}
	if _, err := repo.Get(t.Context(), held); err != nil {
		t.Errorf("the sweep deleted an entry another actor holds: %v", err)
	}

	claim.release()
	if _, err := application.PruneHistory(t.Context()); err != nil {
		t.Fatalf("second PruneHistory: %v", err)
	}
	if _, err := repo.Get(t.Context(), held); err == nil {
		t.Error("a later sweep did not take the entry once it was free")
	}
}
