package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
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
	nw, nf := durabilityRowCounts(t, application, jobID)
	if nw != 0 || nf != 0 {
		t.Errorf("%s: %s kept %d written_articles rows and %d job_files rows, want none",
			why, jobID, nw, nf)
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
	nw, nf := durabilityRowCounts(t, application, queued.ID())
	if nw != 1 || nf != 1 {
		t.Errorf("the queued job kept %d written_articles rows and %d job_files rows, want 1 of each",
			nw, nf)
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
	application.durable = failingRuleStore{durabilityStore: application.durable, err: errors.New("database is locked")}
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
// keeps its job's record rows for a retry; once it is marked completed there
// is no retry, and nothing else would ever remove them.
func TestMarkHistoryCompleted_ReclaimsTheFailedEntrysRuns(t *testing.T) {
	t.Parallel()
	const nArticles = 2
	application, j := newDurabilityTestApp(t, 1, nArticles)
	seedDurability(t, application, j.ID())
	failJobIntoHistory(t, application, j, nArticles)
	rec := &recordingEmitter{}
	application.emitter = rec

	if err := application.MarkHistoryCompleted(t.Context(), j.ID()); err != nil {
		t.Fatalf("MarkHistoryCompleted: %v", err)
	}
	assertRowsGone(t, application, j.ID(), "a failed entry marked completed")
	if !slices.ContainsFunc(rec.events, func(e Event) bool { return e.Type == "history_updated" }) {
		t.Errorf("events = %+v, want a history_updated among them; the UI re-reads history "+
			"on that event alone (ui/src/lib/stores/history.svelte.ts), so without it the "+
			"entry stays Failed on screen until the page is reloaded", rec.events)
	}
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
// departure: the entry goes in history's own transaction, and the rows it kept
// for a retry go through reclaim after it.
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
		t.Errorf("the queued job kept %d written_articles rows and %d job_files rows across the sweep, want 1 and 1", nr, nf)
	}
	if !fileExists(t, queuedManifest) {
		t.Error("the startup sweep unlinked a queued job's manifest")
	}
}

// TestStart_SweepPreservesAFailedHistoryEntrysRuns pins the startup sweep's
// other exception: an orphan whose job is recorded as a FAILED history entry
// must not lose the written_articles and job_files rows a retry verifies
// (internal/durability/reclaim.go's keptForFailedEntry).
// TestStart_SweepsWhatNoDepartureReclaimed above already pins that the
// startup sweep takes a plain orphan and leaves a queued job alone; this is
// the third state, exercised through the same Start() path rather than
// Store.SweepOrphans directly.
func TestStart_SweepPreservesAFailedHistoryEntrysRuns(t *testing.T) {
	application, _, _ := newLifecycleTestApp(t)
	const failedID = "failedhist00000"
	seedDurability(t, application, failedID)
	if err := application.historyRepo.Add(t.Context(), history.Entry{
		NzoID: failedID, Name: failedID, Status: string(constants.StatusFailed),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })

	nr, _ := durabilityRowCounts(t, application, failedID)
	if nr != 1 {
		t.Errorf("a FAILED history entry's written_articles = %d after the startup sweep, want 1: "+
			"a retry verifies these rows against the partial files", nr)
	}
	if nj := jobFilesCount(t, application, failedID); nj != 1 {
		t.Errorf("a FAILED history entry's job_files = %d after the startup sweep, want 1: "+
			"a retry names its rows' files from these", nj)
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
		application.durable = failingRuleStore{durabilityStore: application.durable, err: errors.New("database is locked")}
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

// TestUnlinkDepartedManifests_TakesOnlyWhatTheDispatcherLetGo pins the disk
// half of the rule directly, because both callers reach it and neither can
// show its "nothing to unlink" branches: reclaim always runs the rows first,
// and the sweep only ever passes ids it read off disk.
func TestUnlinkDepartedManifests_TakesOnlyWhatTheDispatcherLetGo(t *testing.T) {
	t.Parallel()
	application, queued := newDurabilityTestApp(t, 1, 1)
	held := placeManifest(t, application, queued.ID())
	departed := placeManifest(t, application, "departed0000000")
	var logged bytes.Buffer
	application.log = slog.New(slog.NewTextHandler(&logged, nil))

	// "nosuchjob0000000" has no manifest: a job whose departure already
	// unlinked it, which the sweep cannot distinguish from one that never had
	// one. Neither may be reported as a failure.
	application.unlinkDepartedManifests([]string{queued.ID(), "departed0000000", "nosuchjob0000000"})

	if !fileExists(t, held) {
		t.Errorf("the manifest of %s went while the dispatcher still holds the job; the "+
			"next tick writes its progress to a job whose manifest is gone", queued.ID())
	}
	if fileExists(t, departed) {
		t.Error("a departed job's manifest survived, so the admin directory keeps growing " +
			"with jobs no queue row reaches")
	}
	if strings.Contains(logged.String(), "could not unlink") {
		t.Errorf("a manifest that was already gone was reported as a failure; log = %q",
			logged.String())
	}
}

// TestSweepOrphans_PreservesManifestOfUnrestoredQueueRow pins that when a
// dispatch_jobs row is skipped during Store.Load or Dispatcher.restore (so the
// row remains in dispatch_jobs while the dispatcher does not hold it), the
// startup orphan sweep preserves both its durability rows and its manifest file
// on disk.
func TestSweepOrphans_PreservesManifestOfUnrestoredQueueRow(t *testing.T) {
	t.Parallel()
	application, queued := newDurabilityTestApp(t, 1, 1)
	const skippedID = "skipped00000000"
	const orphanID = "orphan000000000"

	// Insert a corrupt dispatch_jobs row directly into SQLite (state = 256,
	// which Store.Load skips while leaving the row in dispatch_jobs).
	raw := application.historyRepo.DB()
	if _, err := raw.ExecContext(t.Context(),
		`INSERT INTO dispatch_jobs (id, sort_key, name, state) VALUES (?, 99, 'skipped', 256)`,
		skippedID,
	); err != nil {
		t.Fatalf("insert skipped row: %v", err)
	}
	seedDurability(t, application, skippedID)
	seedDurability(t, application, orphanID)
	skippedManifest := placeManifest(t, application, skippedID)
	orphanManifest := placeManifest(t, application, orphanID)

	if application.hasUnrestoredQueueRow(queued.ID()) {
		t.Error("hasUnrestoredQueueRow returned true for a job registered in the dispatcher")
	}
	if !application.hasUnrestoredQueueRow(skippedID) {
		t.Error("hasUnrestoredQueueRow returned false for a skipped row present in dispatch_jobs")
	}
	if application.hasUnrestoredQueueRow(orphanID) {
		t.Error("hasUnrestoredQueueRow returned true for an orphan with no dispatch_jobs row")
	}

	application.sweepOrphans(t.Context())

	if !fileExists(t, skippedManifest) {
		t.Error("sweepOrphans deleted the manifest of a skipped row that still exists in dispatch_jobs")
	}
	nw, nf := durabilityRowCounts(t, application, skippedID)
	if nw != 1 || nf != 1 {
		t.Errorf("skipped row kept %d written_articles rows and %d job_files rows, want 1 of each", nw, nf)
	}
	if fileExists(t, orphanManifest) {
		t.Error("sweepOrphans kept the manifest of an orphan with no dispatch_jobs row")
	}
	assertRowsGone(t, application, orphanID, "an orphan with no dispatch_jobs row")

	// When dispatch_jobs cannot be queried, hasUnrestoredQueueRow fails closed
	// (returns true) and logs a warning so a transient database fault never
	// unlinks a skipped job's manifest.
	var logged bytes.Buffer
	application.log = slog.New(slog.NewTextHandler(&logged, nil))
	const dbErrID = "dberr0000000000"
	dbErrManifest := placeManifest(t, application, dbErrID)
	if _, err := raw.ExecContext(t.Context(), `DROP TABLE dispatch_jobs`); err != nil {
		t.Fatalf("drop dispatch_jobs: %v", err)
	}
	application.unlinkDepartedManifests([]string{dbErrID})
	if !fileExists(t, dbErrManifest) {
		t.Error("unlinkDepartedManifests unlinked a manifest when dispatch_jobs lookup failed; want fail-closed preservation")
	}
	if !strings.Contains(logged.String(), "could not check dispatch_jobs before unlinking manifest; keeping manifest") {
		t.Errorf("log = %q, want warning about dispatch_jobs lookup failure", logged.String())
	}
}
