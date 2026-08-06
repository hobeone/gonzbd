package queue

import (
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
)

// TestRetainedMatchesManifest_RejectsOutOfOrderIndexes pins the file-index
// check independently of the article-count one.
//
// RestoreRetryProgress indexes job.progress.files by RetainedFile.FileIndex,
// so a row claiming an index other than its own position would write one
// file's progress onto another — or panic out of range. HistoryFileProgress
// orders by file_index, so this only arises when rows are missing or
// duplicated, which is exactly when guessing is worst.
func TestRetainedMatchesManifest_RejectsOutOfOrderIndexes(t *testing.T) {
	j := makeMultiFileJob(t, "guard", 2, 3)
	m := j.manifest

	if !retainedMatchesManifest([]RetainedFile{
		{FileIndex: 0, ArticleCount: 3},
		{FileIndex: 1, ArticleCount: 3},
	}, m) {
		t.Error("well-formed rows were rejected")
	}
	// Right count of rows, right article counts, wrong positions.
	if retainedMatchesManifest([]RetainedFile{
		{FileIndex: 1, ArticleCount: 3},
		{FileIndex: 0, ArticleCount: 3},
	}, m) {
		t.Error("rows whose FileIndex does not match their position were accepted")
	}
	// A gap: file 1 never got a row, so file 2's landed at position 1.
	if retainedMatchesManifest([]RetainedFile{
		{FileIndex: 0, ArticleCount: 3},
		{FileIndex: 2, ArticleCount: 3},
	}, m) {
		t.Error("rows with a gap in file_index were accepted")
	}
	if retainedMatchesManifest([]RetainedFile{{FileIndex: 0, ArticleCount: 3}}, m) {
		t.Error("too few rows were accepted")
	}
}

// TestRestoreRetryProgress_NoProgressOrManifest pins that a job missing
// either residency tier is reported as not-applied rather than panicking.
// RestoreRetryProgress runs on a job built outside the queue, so it cannot
// rely on Add's residency repair having happened yet.
func TestRestoreRetryProgress_NoProgressOrManifest(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)

	if applied, err := store.RestoreRetryProgress(t.Context(), nil); applied || err != nil {
		t.Errorf("nil job: applied=%v err=%v, want false/nil", applied, err)
	}
	if applied, err := store.RestoreRetryProgress(t.Context(), &Job{ID: "bare000000000001"}); applied || err != nil {
		t.Errorf("bare job: applied=%v err=%v, want false/nil", applied, err)
	}
}

// TestRestoreRetryProgress_RestoresCompleteAndDeferredFiles pins the two
// per-file flags the overlay carries besides the article bitmap.
//
// Complete short-circuits to marking every article of the file done, and
// Deferred marks a par2 recovery volume the job chose not to download. A
// retry that lost Deferred would fetch volumes on-demand par2 exists to
// avoid; one that lost Complete would refetch a file already assembled.
func TestRestoreRetryProgress_RestoresCompleteAndDeferredFiles(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	q := New(WithStore(store))

	job := makeMultiFileJob(t, "flags", 2, 2)
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	q.PromoteNext(t.Context())
	if err := q.MarkArticlesDone(job.ID, []string{articleID(0, 0), articleID(0, 1)}); err != nil {
		t.Fatalf("MarkArticlesDone: %v", err)
	}
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}
	job.progress.files[1].Fetch = FetchIfNeeded
	job.progress.files[0].Filename = "assembled.bin"
	if err := q.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.MoveToHistory(t.Context(), job, history.Entry{
		NzoID: job.ID, Name: job.Name, Status: string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	rebuilt := makeMultiFileJob(t, "flags", 2, 2)
	rebuilt.ID = job.ID
	applied, err := store.RestoreRetryProgress(t.Context(), rebuilt)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if !applied {
		t.Fatal("overlay refused for a matching manifest")
	}
	p := rebuilt.Progress()
	if !p.FileComplete(0) {
		t.Error("file 0 lost its Complete flag; the retry will reassemble it")
	}
	if !p.ArticleDone(0) || !p.ArticleDone(1) {
		t.Error("a complete file's articles should all come back done")
	}
	if p.FileFetchPolicy(1) != FetchIfNeeded {
		t.Error("file 1 lost its Deferred flag; on-demand par2 would fetch it anyway")
	}
	if p.FileFilename(0) != "assembled.bin" {
		t.Errorf("FileFilename(0) = %q, want the resolved name carried over", p.FileFilename(0))
	}
}

// TestRestoreRetryProgress_DoesNotClearAHoldTheRebuiltJobJustSet pins the
// divergence RestoreJobProgress's unconditional-assign shape cannot use
// here: NewJob rebuilding the retried job from the re-parsed NZB has
// already set FetchIfNeeded on every recovery volume when OnDemandPar2 is
// on, but the row this overlay reads can legitimately say FetchAlways — a
// prior run's undeferRecoveryLocked release, persisted before the job
// failed and moved to history. Unlike RestoreJobProgress (whose progress
// was itself sized from the same rows via ArticleCountsByJob, so row and
// memory cannot disagree there), row and memory can disagree here, and the
// row must not win: a retry that adopted FetchAlways from a stale row would
// silently clear a hold the rebuild just decided on, skipping the CRC
// release path entirely and pulling the volume's bytes into every reported
// figure for a job that never actually needed it downloaded.
func TestRestoreRetryProgress_DoesNotClearAHoldTheRebuiltJobJustSet(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	q := New(WithStore(store))

	// par2NZB (see ondemand_par2_test.go): file 0 content, file 1 par2 index,
	// file 2 recovery volume.
	job, err := NewJob(par2NZB(), AddOptions{Filename: "divergence.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if job.progress.files[2].Fetch != FetchIfNeeded {
		t.Fatalf("fixture guard: recovery volume not held after Add")
	}

	// Simulate a prior run that released the volume (e.g. a permanent
	// article failure via undeferRecoveryLocked) and persisted that release
	// before the job later failed and moved to history.
	job.progress.files[2].Fetch = FetchAlways
	if err := store.Update(t.Context(), job); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := store.MoveToHistory(t.Context(), job, history.Entry{
		NzoID: job.ID, Name: job.Name, Status: string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	// Retry rebuilds the job from the NZB. With OnDemandPar2 still on, NewJob
	// holds the recovery volume again before the overlay ever runs.
	rebuilt, err := NewJob(par2NZB(), AddOptions{Filename: "divergence.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob (rebuilt): %v", err)
	}
	rebuilt.ID = job.ID
	if rebuilt.progress.files[2].Fetch != FetchIfNeeded {
		t.Fatalf("fixture guard: rebuilt job did not hold the recovery volume")
	}

	applied, err := store.RestoreRetryProgress(t.Context(), rebuilt)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if !applied {
		t.Fatal("overlay refused for a matching manifest")
	}
	if got := rebuilt.Progress().FileFetchPolicy(2); got != FetchIfNeeded {
		t.Errorf("FileFetchPolicy(2) = %v, want FetchIfNeeded — the overlay cleared a hold the rebuild just set, from a stale FetchAlways row", got)
	}
}

// TestRestoreRetryProgress_DowngradesRetainedFetchNever pins the other half
// of the overlay's fetch-policy handling, complementing
// TestRestoreRetryProgress_DoesNotClearAHoldTheRebuiltJobJustSet: a row that
// says FetchNever (the oracle discarded the volume on the run that failed)
// must land back on FetchIfNeeded, the same downgrade ResetForRetry applies
// to a live job — the retry is about to re-fetch different content, so the
// old verdict cannot be trusted.
//
// The rebuild runs with OnDemandPar2 off, so NewJob's own fresh default for
// the recovery volume is FetchAlways, not FetchIfNeeded. That is deliberate:
// if the overlay's FetchNever branch were dropped, fp would silently keep
// NewJob's FetchAlways default and this test would not notice the loss —
// only forcing fp away from what the overlay must supply makes the branch
// load-bearing rather than incidentally covered.
func TestRestoreRetryProgress_DowngradesRetainedFetchNever(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	q := New(WithStore(store))

	// par2NZB (see ondemand_par2_test.go): file 0 content, file 1 par2 index,
	// file 2 recovery volume.
	job, err := NewJob(par2NZB(), AddOptions{Filename: "discard.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if job.progress.files[2].Fetch != FetchIfNeeded {
		t.Fatalf("fixture guard: recovery volume not held after Add")
	}

	// Simulate the oracle ruling the volume unnecessary before the job later
	// failed (on an unrelated file) and moved to history.
	job.progress.files[2].Fetch = FetchNever
	if err := store.Update(t.Context(), job); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := store.MoveToHistory(t.Context(), job, history.Entry{
		NzoID: job.ID, Name: job.Name, Status: string(constants.StatusFailed),
	}); err != nil {
		t.Fatalf("MoveToHistory: %v", err)
	}

	// Retry rebuilds the job with on-demand par2 now off, so NewJob leaves
	// the recovery volume at its zero-value FetchAlways rather than holding
	// it — isolating the overlay's own downgrade from NewJob's.
	rebuilt, err := NewJob(par2NZB(), AddOptions{Filename: "discard.nzb", OnDemandPar2: false}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob (rebuilt): %v", err)
	}
	rebuilt.ID = job.ID
	if rebuilt.progress.files[2].Fetch != FetchAlways {
		t.Fatalf("fixture guard: rebuilt job did not default the recovery volume to FetchAlways")
	}

	applied, err := store.RestoreRetryProgress(t.Context(), rebuilt)
	if err != nil {
		t.Fatalf("RestoreRetryProgress: %v", err)
	}
	if !applied {
		t.Fatal("overlay refused for a matching manifest")
	}
	if got := rebuilt.Progress().FileFetchPolicy(2); got != FetchIfNeeded {
		t.Errorf("FileFetchPolicy(2) = %v, want FetchIfNeeded — a discarded verdict must be re-derived, not left as FetchAlways nor stuck at FetchNever", got)
	}
}

// TestRestoreRetryProgress_QueryFailurePropagates pins that a broken read is
// an error rather than a silent "nothing retained". The two are opposite
// instructions: one says re-download the job, the other says the database is
// unusable, and conflating them would quietly refetch everything.
func TestRestoreRetryProgress_QueryFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)
	store := NewSQLiteStore(repo.DB(), dir, repo)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	job := makeMultiFileJob(t, "broken", 1, 2)
	applied, err := store.RestoreRetryProgress(t.Context(), job)
	if err == nil {
		t.Fatal("a failed query was reported as success")
	}
	if applied {
		t.Error("applied is true despite the error")
	}
}
