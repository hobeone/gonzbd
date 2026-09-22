package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// placeManifest puts a placeholder manifest on disk for jobID and returns
// its path. reclaim decides whether to unlink it; its content is irrelevant.
func placeManifest(t *testing.T, application *Application, jobID string) string {
	t.Helper()
	dir := manifestDir(application.config.GetGeneral().AdminDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, jobID+manifestSuffix)
	if err := os.WriteFile(path, []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// assertRowsGone fails unless jobID has no row in any per-job table.
func assertRowsGone(t *testing.T, application *Application, jobID, why string) {
	t.Helper()
	nr, nf := durabilityRowCounts(t, application, jobID)
	if nj := jobFilesCount(t, application, jobID); nr != 0 || nf != 0 || nj != 0 {
		t.Errorf("%s: %s kept %d runs, %d failed rows and %d job_files rows, want none",
			why, jobID, nr, nf, nj)
	}
}

// TestReclaim_TakesOnlyWhatNothingReaches pins the helper's two halves
// against a queued job and a departed one: the rule takes the departed job's
// rows, and the manifest goes only for the job the dispatcher does not hold.
func TestReclaim_TakesOnlyWhatNothingReaches(t *testing.T) {
	t.Parallel()
	application, queued := newDurabilityTestApp(t, 1, 2)
	const gone = "departed0000000"
	for _, id := range []string{queued.ID(), gone} {
		seedDurability(t, application, id)
	}
	queuedManifest := placeManifest(t, application, queued.ID())
	goneManifest := placeManifest(t, application, gone)

	application.reclaim(t.Context(), queued.ID(), gone)

	assertRowsGone(t, application, gone, "a job nothing reaches")
	if fileExists(t, goneManifest) {
		t.Error("the departed job's manifest survived reclaim")
	}
	nr, nf := durabilityRowCounts(t, application, queued.ID())
	if nj := jobFilesCount(t, application, queued.ID()); nr != 1 || nf != 1 || nj != 1 {
		t.Errorf("the queued job kept %d runs, %d failed rows and %d job_files rows, want 1 of each",
			nr, nf, nj)
	}
	if !fileExists(t, queuedManifest) {
		t.Error("reclaim unlinked the manifest of a job the dispatcher still holds; " +
			"appResidency.hydrate cannot load it without one")
	}
}

// TestReclaim_LogsAFailureAndStillUnlinksTheManifest pins the helper's
// failure policy. Every caller has already departed or is already failing,
// so the error is logged rather than returned — and logged it must be, or a
// failed reclaim leaves rows with no trace until the next start.
func TestReclaim_LogsAFailureAndStillUnlinksTheManifest(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	var logged bytes.Buffer
	application.log = slog.New(slog.NewTextHandler(&logged, nil))
	application.durable = failingRunStore{durabilityStore: application.durable, err: errors.New("database is locked")}
	manifest := placeManifest(t, application, "departed0000000")

	application.reclaim(t.Context(), "departed0000000")

	if !strings.Contains(logged.String(), "could not reclaim") || !strings.Contains(logged.String(), "database is locked") {
		t.Errorf("a failed reclaim was not logged with its cause; log = %q", logged.String())
	}
	if fileExists(t, manifest) {
		t.Error("the manifest of a job the dispatcher does not hold survived; the " +
			"file does not depend on the rows' reclaim succeeding")
	}
}

// TestReclaim_UnlinksManifestsWithoutAHistoryDatabase pins the degraded mode:
// with no store there are no rows, but a departed job still has a manifest.
func TestReclaim_UnlinksManifestsWithoutAHistoryDatabase(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	application.durable = nil
	manifest := placeManifest(t, application, "departed0000000")

	application.reclaim(t.Context(), "departed0000000")

	if fileExists(t, manifest) {
		t.Error("the manifest survived reclaim with no history database")
	}
}

// TestRemoveJob_ReclaimsAJobSomeoneElseRemoved is E8. RemoveJob cancels a job,
// and the tick can evict a cancelled job that never ran before RemoveJob's own
// Remove reaches it. Remove then reports ErrNotFound; RemoveJob must still
// reclaim, or the rows and manifest wait for the next start, and must report
// success, because the job it was asked to remove is gone.
func TestRemoveJob_ReclaimsAJobSomeoneElseRemoved(t *testing.T) {
	t.Parallel()
	application, j := newDurabilityTestApp(t, 1, 2)
	seedDurability(t, application, j.ID())
	manifest := placeManifest(t, application, j.ID())
	application.removeJobHook = func(id string) {
		if err := application.dispatcher.Remove(context.Background(), id); err != nil {
			t.Errorf("fixture: the peer removal failed: %v", err)
		}
	}

	if err := application.RemoveJob(t.Context(), j.ID(), false); err != nil {
		t.Fatalf("RemoveJob = %v, want nil: the job the caller asked to remove is gone", err)
	}
	assertRowsGone(t, application, j.ID(), "a job removed under RemoveJob")
	if fileExists(t, manifest) {
		t.Error("the manifest of a job removed under RemoveJob survived")
	}
}

// TestRemoveJob_ReclaimsAJobThatLeftTheQueueBeforeTheCall is the other half of
// E8. A Remove that fails leaves the job registered and cancelled; the next
// tick evicts it, and a user who asks again finds no job. The rows and the
// manifest are still there, and this call is what asks for them.
func TestRemoveJob_ReclaimsAJobThatLeftTheQueueBeforeTheCall(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	const gone = "departed0000000"
	seedDurability(t, application, gone)
	manifest := placeManifest(t, application, gone)

	err := application.RemoveJob(t.Context(), gone, false)
	if err == nil {
		t.Error("RemoveJob = nil for a job that is not in the queue; the caller is told it removed one")
	}
	assertRowsGone(t, application, gone, "a job that had already left the queue")
	if fileExists(t, manifest) {
		t.Error("the manifest of a job that had already left the queue survived")
	}
}

// TestMarkHistoryCompleted_ReclaimsTheFailedEntrysRuns is E9. A FAILED entry
// keeps its job's durable_runs for a retry; once it is marked completed there
// is no retry, and nothing else would ever remove them.
func TestMarkHistoryCompleted_ReclaimsTheFailedEntrysRuns(t *testing.T) {
	t.Parallel()
	const nArticles = 2
	application, j := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, j.ID())
	failJobIntoHistory(t, application, j, nArticles)

	if err := application.MarkHistoryCompleted(t.Context(), j.ID()); err != nil {
		t.Fatalf("MarkHistoryCompleted: %v", err)
	}
	assertRowsGone(t, application, j.ID(), "a failed entry marked completed")
	entry, err := application.historyRepo.Get(t.Context(), j.ID())
	if err != nil {
		t.Fatal(err)
	}
	if entry.Status != "Completed" {
		t.Errorf("status = %q, want Completed", entry.Status)
	}
}

// TestMarkHistoryCompleted_ReportsAMissingEntry pins that the not-found answer
// reaches the API, which reports it rather than a success.
func TestMarkHistoryCompleted_ReportsAMissingEntry(t *testing.T) {
	t.Parallel()
	application, _, _ := newLifecycleTestApp(t)
	if err := application.MarkHistoryCompleted(t.Context(), "nosuchentry0000"); !errors.Is(err, history.ErrNotFound) {
		t.Errorf("err = %v, want history.ErrNotFound", err)
	}
	application.historyRepo = nil
	if err := application.MarkHistoryCompleted(t.Context(), "nosuchentry0000"); err == nil {
		t.Error("MarkHistoryCompleted with no history repository = nil, want an error")
	}
}

// TestRemoveHistoryJob_ReclaimsTheFailedEntrysRuns pins the history-delete
// departure: the entry and its retained file progress go in history's own
// transaction, and the runs it kept for a retry go through reclaim after it.
func TestRemoveHistoryJob_ReclaimsTheFailedEntrysRuns(t *testing.T) {
	t.Parallel()
	const nArticles = 2
	application, j := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, j.ID())
	failJobIntoHistory(t, application, j, nArticles)

	if err := application.RemoveHistoryJob(t.Context(), j.ID(), false); err != nil {
		t.Fatalf("RemoveHistoryJob: %v", err)
	}
	assertRowsGone(t, application, j.ID(), "a deleted failed entry")
	var n int
	if err := application.historyRepo.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM history_job_files WHERE job_id = ?`, j.ID()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d history_job_files rows survive their entry", n)
	}
}

// TestStart_SweepsWhatNoDepartureReclaimed pins the backstop: rows and a
// manifest a crash stranded between a departure and its reclaim are taken at
// the next start, and a queued job's are not.
func TestStart_SweepsWhatNoDepartureReclaimed(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: "kept.bin", Bytes: 100,
		Articles: []nzb.Article{{ID: "kept0@t", Bytes: 100, Number: 1}},
	}}}
	queued, hdr, err := BuildIngestJob(application.config, parsed, "kept.nzb", types.FetchOptions{NzbName: "kept"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Dispatcher().Add(context.Background(), queued, hdr); err != nil {
		t.Fatal(err)
	}
	const stranded = "stranded0000000"
	for _, id := range []string{queued.ID(), stranded} {
		seedDurability(t, application, id)
	}
	queuedManifest := writeManifestFor(t, application, queued)
	strandedManifest := placeManifest(t, application, stranded)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })

	assertRowsGone(t, application, stranded, "a job no departure reclaimed")
	if fileExists(t, strandedManifest) {
		t.Error("the stranded manifest survived the startup sweep")
	}
	if nr, nf := durabilityRowCounts(t, application, queued.ID()); nr != 1 || nf != 1 {
		t.Errorf("the queued job kept %d runs and %d failed rows across the sweep, want 1 and 1", nr, nf)
	}
	if !fileExists(t, queuedManifest) {
		t.Error("the startup sweep unlinked a queued job's manifest")
	}
}

// writeManifestFor writes a job's real manifest, as AddJob does, so the job
// can still be hydrated after Start.
func writeManifestFor(t *testing.T, application *Application, j *job.Job) string {
	t.Helper()
	m, err := j.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	adminDir := application.config.GetGeneral().AdminDir
	if err := os.MkdirAll(manifestDir(adminDir), 0o750); err != nil {
		t.Fatal(err)
	}
	path, err := manifestPath(adminDir, j.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteGzAtomicBytes(path, data); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSweepOrphans_ReportsWhatItCouldNotSweep pins the sweep's two failure
// paths. Each is logged and neither stops the other half: a sweep that cannot
// reach the rows still clears the manifests, and one that cannot list the
// manifests still cleared the rows.
func TestSweepOrphans_ReportsWhatItCouldNotSweep(t *testing.T) {
	t.Parallel()

	t.Run("rows", func(t *testing.T) {
		t.Parallel()
		application, _, _ := newLifecycleTestApp(t)
		var logged bytes.Buffer
		application.log = slog.New(slog.NewTextHandler(&logged, nil))
		application.durable = failingRunStore{durabilityStore: application.durable, err: errors.New("database is locked")}
		manifest := placeManifest(t, application, "stranded0000000")

		application.sweepOrphans(t.Context())

		if !strings.Contains(logged.String(), "startup sweep of unreachable durability rows failed") {
			t.Errorf("a failed row sweep was not logged; log = %q", logged.String())
		}
		if fileExists(t, manifest) {
			t.Error("the manifest sweep did not run after the row sweep failed")
		}
	})

	t.Run("manifests", func(t *testing.T) {
		t.Parallel()
		application, _, _ := newLifecycleTestApp(t)
		var logged bytes.Buffer
		application.log = slog.New(slog.NewTextHandler(&logged, nil))
		seedDurability(t, application, "stranded0000000")
		// A file where the manifests directory should be: listing it fails
		// with something other than not-exist.
		dir := manifestDir(application.config.GetGeneral().AdminDir)
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		application.sweepOrphans(t.Context())

		if !strings.Contains(logged.String(), "could not list the manifests") {
			t.Errorf("a failed manifest listing was not logged; log = %q", logged.String())
		}
		assertRowsGone(t, application, "stranded0000000", "a row sweep beside a failed manifest listing")
	})

	t.Run("no manifests directory", func(t *testing.T) {
		t.Parallel()
		application, _, _ := newLifecycleTestApp(t)
		var logged bytes.Buffer
		application.log = slog.New(slog.NewTextHandler(&logged, nil))

		application.sweepOrphans(t.Context())

		if strings.Contains(logged.String(), "manifests") {
			t.Errorf("a missing manifests directory was reported as a failure; log = %q", logged.String())
		}
	})
}
