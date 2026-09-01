package queue

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// minimalNZB returns the smallest valid parsed NZB for testing.
func minimalNZB() *nzb.NZB {
	return &nzb.NZB{
		Files: []nzb.File{
			{Subject: "test", Bytes: 100, Articles: []nzb.Article{{ID: "a@b", Bytes: 100, Number: 1}}},
		},
	}
}

func TestNewJob_CategoryPPInherit(t *testing.T) {
	t.Parallel()
	cats := []config.CategoryConfig{
		{Name: "tv", PP: 3, Script: "tv.sh", Priority: int(constants.HighPriority)},
	}
	job, err := NewJob(minimalNZB(), AddOptions{
		Filename:   "test.nzb",
		Category:   "tv",
		PP:         types.PPInherit,
		Categories: cats,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.PP != 3 {
		t.Errorf("PP = %d, want 3 (from category)", job.PP)
	}
	if job.Script != "tv.sh" {
		t.Errorf("Script = %q, want %q (from category)", job.Script, "tv.sh")
	}
}

func TestNewJob_CategoryPriorityInherit(t *testing.T) {
	t.Parallel()
	cats := []config.CategoryConfig{
		{Name: "movies", Priority: int(constants.HighPriority)},
	}
	job, err := NewJob(minimalNZB(), AddOptions{
		Filename:   "test.nzb",
		Category:   "movies",
		Priority:   constants.DefaultPriority,
		Categories: cats,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Priority != constants.HighPriority {
		t.Errorf("Priority = %d, want %d (HighPriority from category)", job.Priority, constants.HighPriority)
	}
}

func TestNewJob_ExplicitOverridesCategory(t *testing.T) {
	t.Parallel()
	cats := []config.CategoryConfig{
		{Name: "tv", PP: 3, Script: "tv.sh", Priority: int(constants.LowPriority)},
	}
	job, err := NewJob(minimalNZB(), AddOptions{
		Filename:   "test.nzb",
		Category:   "tv",
		PP:         3,
		Script:     "custom.sh",
		Priority:   constants.HighPriority,
		Categories: cats,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.PP != 3 {
		t.Errorf("PP = %d, want 3 (explicit override)", job.PP)
	}
	if job.Script != "custom.sh" {
		t.Errorf("Script = %q, want %q (explicit override)", job.Script, "custom.sh")
	}
	if job.Priority != constants.HighPriority {
		t.Errorf("Priority = %d, want %d (explicit override)", job.Priority, constants.HighPriority)
	}
}

func TestNewJob_ClampsPPAbove3(t *testing.T) {
	t.Parallel()
	// 4e6d545: legacy bitmask PP values (e.g. 7) are clamped to the max valid
	// level 3, not passed through. Without the clamp, job.PP would be 7.
	job, err := NewJob(minimalNZB(), AddOptions{
		Filename: "test.nzb",
		PP:       7,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.PP != 3 {
		t.Errorf("job.PP = %d, want 3 (PP>3 clamped)", job.PP)
	}
}

func TestNewJob_NoCategoriesFallback(t *testing.T) {
	t.Parallel()
	// No Categories: sentinels resolve through FindCategory which
	// falls back to BuiltinDefaultCategory (PP=3, Priority=Normal).
	// This must NEVER return PP=0 — that would silently disable
	// post-processing.
	job, err := NewJob(minimalNZB(), AddOptions{
		Filename: "test.nzb",
		PP:       types.PPInherit,
		Priority: constants.DefaultPriority,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := config.BuiltinDefaultCategory()
	if job.PP != want.PP {
		t.Errorf("PP = %d, want %d (builtin baseline)", job.PP, want.PP)
	}
	if job.PP == 0 {
		t.Errorf("PP must not be 0 — that silently disables post-processing")
	}
	if job.Priority != constants.NormalPriority {
		t.Errorf("Priority = %d, want %d (builtin baseline)", job.Priority, constants.NormalPriority)
	}
}

func TestNewJob_CategoryFallbackToDefault(t *testing.T) {
	t.Parallel()
	cats := []config.CategoryConfig{
		{Name: "Default", PP: 2, Script: "fallback.sh"},
	}
	job, err := NewJob(minimalNZB(), AddOptions{
		Filename:   "test.nzb",
		Category:   "nonexistent",
		PP:         types.PPInherit,
		Categories: cats,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.PP != 2 {
		t.Errorf("PP = %d, want 2 (from Default category)", job.PP)
	}
	if job.Script != "fallback.sh" {
		t.Errorf("Script = %q, want %q (from Default category)", job.Script, "fallback.sh")
	}
}

// ---------- IsEarlyAbort ----------

func TestIsEarlyAbort_NotEnoughSamples(t *testing.T) {
	t.Parallel()
	j := &Job{progress: &JobProgress{articlesResolved: 5, articlesFailed: 5}}
	if j.IsEarlyAbort() {
		t.Error("fired with only 5 resolved articles, need 10")
	}
}

func TestIsEarlyAbort_HighFailRate(t *testing.T) {
	t.Parallel()
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 8}} // 80%
	if !j.IsEarlyAbort() {
		t.Error("should fire at 80% failure rate with 10 resolved")
	}
	if !j.progress.earlyAborted {
		t.Error("EarlyAborted flag should be set")
	}
}

func TestIsEarlyAbort_UnderThreshold(t *testing.T) {
	t.Parallel()
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 7}} // 70%
	if j.IsEarlyAbort() {
		t.Error("should not fire at 70% failure rate")
	}
}

func TestIsEarlyAbort_OnlyFiresOnce(t *testing.T) {
	t.Parallel()
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 10}}
	if !j.IsEarlyAbort() {
		t.Fatal("first call should fire")
	}
	if j.IsEarlyAbort() {
		t.Error("second call should not fire (already aborted)")
	}
}

func TestIsEarlyAbort_ExactThreshold(t *testing.T) {
	t.Parallel()
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 8}} // exactly 80%
	if !j.IsEarlyAbort() {
		t.Error("should fire at exactly 80%")
	}
}

func TestIsEarlyAbort_AllFailed(t *testing.T) {
	t.Parallel()
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 10}} // 100%
	if !j.IsEarlyAbort() {
		t.Error("should fire at 100% failure rate")
	}
}

// TestRecomputePending_SeedsEarlyAbortCounters proves TRACE-3:
// recomputePending must restore ArticlesResolved/ArticlesFailed from
// ground truth (the persisted Done/Failed article flags), not leave them
// at their zero value. Before the fix, a reload after a restart forgot
// how many articles had already resolved/failed, corrupting the
// IsEarlyAbort heuristic for jobs resumed mid-download.
func TestRecomputePending_SeedsEarlyAbortCounters(t *testing.T) {
	t.Parallel()
	files := []JobFile{
		{Articles: []JobArticle{
			{ID: "a1", Bytes: 100},
			{ID: "a2", Bytes: 100},
			{ID: "a3", Bytes: 100},
			{ID: "a4", Bytes: 100}, // still pending
		}},
	}
	j := &Job{manifest: newManifest(files)}
	j.progress = newJobProgress(j.manifest)
	j.progress.done.Set(0) // resolved, succeeded
	j.progress.done.Set(1)
	j.progress.failed.Set(1) // resolved, failed
	j.progress.done.Set(2)
	j.progress.failed.Set(2) // resolved, failed
	// Simulate the state right after a JSON unmarshal from disk: the
	// excluded-from-JSON transient counters are zero even though the
	// persisted article flags above record 3 already-resolved articles
	// (2 failed).
	j.progress.articlesResolved = 0
	j.progress.articlesFailed = 0

	j.progress.recompute(j.manifest)

	if j.progress.articlesResolved != 3 {
		t.Errorf("ArticlesResolved = %d, want 3 (count of Done articles)", j.progress.articlesResolved)
	}
	if j.progress.articlesFailed != 2 {
		t.Errorf("ArticlesFailed = %d, want 2 (count of Done&&Failed articles)", j.progress.articlesFailed)
	}
}

// TestRecomputePending_EarlyAbortFiresAfterReload proves the seeded
// counters correctly feed IsEarlyAbort after a simulated restart: a job
// that had already accumulated 8/10 failures before the restart must
// still trip the early-abort heuristic on the very first post-reload
// check, without needing 10 fresh failures in the new session.
func TestRecomputePending_EarlyAbortFiresAfterReload(t *testing.T) {
	t.Parallel()
	articles := make([]JobArticle, 0, 10)
	for i := range 10 {
		articles = append(articles, JobArticle{ID: fmt.Sprintf("a%d", i), Bytes: 100})
	}
	j := &Job{manifest: newManifest([]JobFile{{Articles: articles}})}
	j.progress = newJobProgress(j.manifest)
	for i := range 10 {
		j.progress.done.Set(i)
		if i < 8 {
			j.progress.failed.Set(i)
		}
	}
	// Simulate post-unmarshal zeroing of the transient counters.
	j.progress.articlesResolved = 0
	j.progress.articlesFailed = 0

	j.progress.recompute(j.manifest)

	if !j.IsEarlyAbort() {
		t.Error("IsEarlyAbort should fire immediately after reload: 8/10 failures were already on disk")
	}
}

func TestJobUnexportedHelpersDirect(t *testing.T) {
	t.Parallel()
	// 1. stripNZBExt
	testsStrip := []struct {
		in   string
		want string
	}{
		{"my_file.nzb", "my_file"},
		{"my_file.nzb.gz", "my_file"},
		{"my_file.nzb.bz2", "my_file"},
		{"my_file.txt", "my_file.txt"},
		{"my_file", "my_file"},
	}
	for _, tc := range testsStrip {
		if got := stripNZBExt(tc.in); got != tc.want {
			t.Errorf("stripNZBExt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// 2. deriveName
	testsDerive := []struct {
		in   string
		want string
	}{
		{"/path/to/file.nzb", "file"},
		{"/path/to/file.nzb.gz", "file"},
		{"/path/to/file.nzb.bz2", "file"},
		{"/path/to/file.rar", "file"},
		{"/path/to/file", "file"},
		{"file", "file"},
	}
	for _, tc := range testsDerive {
		if got := deriveName(tc.in); got != tc.want {
			t.Errorf("deriveName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// 3. newJobID
	id1, err := newJobID()
	if err != nil {
		t.Fatalf("newJobID failed: %v", err)
	}
	if len(id1) != 16 {
		t.Errorf("newJobID length = %d, want 16", len(id1))
	}
	// Verify it is a valid hex string
	for _, c := range id1 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("invalid hex character in job ID: %c", c)
		}
	}

	id2, _ := newJobID()
	if id1 == id2 {
		t.Error("newJobID returned non-unique IDs")
	}
}

func TestNewJob_CategoryPriorityBoundaryClamping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		priority int
		want     constants.Priority
	}{
		{"exactly -128", -128, constants.Priority(-128)},
		{"below -128", -129, constants.NormalPriority},
		{"exactly 127", 127, constants.Priority(127)},
		{"above 127", 128, constants.NormalPriority},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cats := []config.CategoryConfig{
				{Name: "movies", Priority: tc.priority},
			}
			job, err := NewJob(minimalNZB(), AddOptions{
				Filename:   "test.nzb",
				Category:   "movies",
				Priority:   constants.DefaultPriority,
				Categories: cats,
			}, fsutil.SanitizeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if job.Priority != tc.want {
				t.Errorf("Priority = %d, want %d", job.Priority, tc.want)
			}
		})
	}
}

// TestResetForRetry_OnlyTouchesFailedArticles pins ResetForRetry's
// selective-reset contract directly (rather than through app.RetryHistoryJob's
// indirect coverage) on a three-file job where only some files have failed
// articles: file 0 fully succeeded, file 1 partially failed, file 2 fully
// failed. Only the failed articles' done/failed bits should be reset (and
// the per-file/job-level FailedBytes recomputed from what remains), and
// Complete should be cleared only for files that had a reset.
func TestResetForRetry_OnlyTouchesFailedArticles(t *testing.T) {
	t.Parallel()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "f0", Bytes: 300, Articles: []nzb.Article{
			{ID: "f0a0@t", Bytes: 100, Number: 1},
			{ID: "f0a1@t", Bytes: 200, Number: 2},
		}},
		{Subject: "f1", Bytes: 700, Articles: []nzb.Article{
			{ID: "f1a0@t", Bytes: 300, Number: 1},
			{ID: "f1a1@t", Bytes: 400, Number: 2},
		}},
		{Subject: "f2", Bytes: 1500, Articles: []nzb.Article{
			{ID: "f2a0@t", Bytes: 500, Number: 1},
			{ID: "f2a1@t", Bytes: 1000, Number: 2},
		}},
	}}
	job, err := NewJob(parsed, AddOptions{Filename: "retry-reset.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// File 0: both articles succeed, file marked complete.
	ackDone(t, q, job.ID, "f0a0@t", "f0a1@t")
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete(0): %v", err)
	}
	// File 1: one succeeds, one fails.
	ackDone(t, q, job.ID, "f1a0@t")
	ackFailed(t, q, job.ID, "f1a1@t")
	// File 2: both articles fail.
	ackFailed(t, q, job.ID, "f2a0@t", "f2a1@t")

	snap := q.SnapshotJob(job.ID)
	if snap == nil {
		t.Fatalf("SnapshotJob(%s) returned nil", job.ID)
	}
	remainingBeforeReset := snap.Progress().RemainingBytes()

	snap.ResetForRetry()

	if snap.Status != constants.StatusQueued {
		t.Errorf("Status = %q, want Queued", snap.Status)
	}

	// File 0: untouched — both articles were never failed.
	if !snap.Progress().ArticleDone(0) || snap.Progress().ArticleFailed(0) {
		t.Errorf("f0a0: done=%v failed=%v, want done=true failed=false (untouched)",
			snap.Progress().ArticleDone(0), snap.Progress().ArticleFailed(0))
	}
	if !snap.Progress().ArticleDone(1) || snap.Progress().ArticleFailed(1) {
		t.Errorf("f0a1: done=%v failed=%v, want done=true failed=false (untouched)",
			snap.Progress().ArticleDone(1), snap.Progress().ArticleFailed(1))
	}
	if !snap.Progress().FileComplete(0) {
		t.Error("file 0 Complete was cleared, want it to survive since none of its articles were reset")
	}

	// File 1: article index 2 (f1a0) untouched, index 3 (f1a1) reset.
	if !snap.Progress().ArticleDone(2) || snap.Progress().ArticleFailed(2) {
		t.Errorf("f1a0: done=%v failed=%v, want done=true failed=false (untouched)",
			snap.Progress().ArticleDone(2), snap.Progress().ArticleFailed(2))
	}
	if snap.Progress().ArticleDone(3) || snap.Progress().ArticleFailed(3) {
		t.Errorf("f1a1: done=%v failed=%v, want done=false failed=false (reset)",
			snap.Progress().ArticleDone(3), snap.Progress().ArticleFailed(3))
	}
	if snap.Progress().FileComplete(1) {
		t.Error("file 1 Complete should be false after reset")
	}

	// File 2: both articles reset.
	if snap.Progress().ArticleDone(4) || snap.Progress().ArticleFailed(4) {
		t.Errorf("f2a0: done=%v failed=%v, want done=false failed=false (reset)",
			snap.Progress().ArticleDone(4), snap.Progress().ArticleFailed(4))
	}
	if snap.Progress().ArticleDone(5) || snap.Progress().ArticleFailed(5) {
		t.Errorf("f2a1: done=%v failed=%v, want done=false failed=false (reset)",
			snap.Progress().ArticleDone(5), snap.Progress().ArticleFailed(5))
	}
	if snap.Progress().FileComplete(2) {
		t.Error("file 2 Complete should be false after reset (was never true anyway)")
	}

	// RemainingBytes must be re-credited by exactly the reset articles' bytes:
	// f1a1 (400) + f2a0 (500) + f2a1 (1000) = 1900. f0's succeeded bytes and
	// f1a0's succeeded bytes must NOT be re-credited.
	const wantDelta = 400 + 500 + 1000
	if got := snap.Progress().RemainingBytes() - remainingBeforeReset; got != wantDelta {
		t.Errorf("RemainingBytes delta = %d, want %d", got, wantDelta)
	}
}

func TestJobPhase_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		phase JobPhase
		want  string
	}{
		{PhasePending, "Pending"},
		{PhaseActive, "Active"},
		{PhaseProcessing, "Processing"},
		{PhasePaused, "Paused"},
		{PhaseTerminal, "Terminal"},
		{JobPhase(999), "Unknown"},
	}
	for _, tc := range tests {
		if got := tc.phase.String(); got != tc.want {
			t.Errorf("JobPhase(%d).String() = %q, want %q", tc.phase, got, tc.want)
		}
	}
}

// TestJobDeferredRecoveryIndices_NilProgress pins the de-hydrated-job branch
// of Job.DeferredRecoveryIndices: a job with no resident progress (e.g. a
// snapshot of a de-hydrated queued job) must report no deferred indices
// rather than panicking on a nil progress dereference.
func TestJobDeferredRecoveryIndices_NilProgress(t *testing.T) {
	t.Parallel()
	job := &Job{ID: "no-progress"}
	if got := job.DeferredRecoveryIndices(); got != nil {
		t.Errorf("DeferredRecoveryIndices() with nil progress = %v, want nil", got)
	}
}

// TestJobSetters_AreRawWithPolicyOnTheWrapper pins the division of labour
// B2.4a chose, which is the part of these one-line setters that is not
// obvious from reading them.
//
// Each Job setter assigns and nothing else; the validation and sanitization
// stay on the Queue wrapper. That is deliberate in two cases and load-bearing
// in both:
//
//   - setPP does NOT range-check. Queue.SetPP validates BEFORE the lookup, so
//     an invalid level on a missing job reports the level rather than
//     ErrNotFound. Moving the check here would silently swap that precedence.
//   - setName does NOT sanitize. CleanupName and SanitizeFolderName read
//     Queue.sOpts, which a Job cannot reach — so a caller who acquires a *Job
//     and calls setName directly gets the raw string, and any future exported
//     form of it must sanitize at its own door.
//
// Asserting the raw behaviour is what makes a later "tidying" that folds
// policy down into the Job a test failure rather than a silent change.
func TestJobSetters_AreRawWithPolicyOnTheWrapper(t *testing.T) {
	t.Parallel()

	j := &Job{ID: "j1", progress: &JobProgress{}}

	// Out of the 0-3 range Queue.SetPP enforces: the setter takes it anyway.
	j.setPP(99)
	if j.PP != 99 {
		t.Errorf("setPP(99) left PP = %d, want 99 — validation belongs to Queue.SetPP, not here", j.PP)
	}

	// Neither stripped of its extension nor sanitized: Queue.SetName does both.
	const raw = "a/b: name.nzb"
	j.setName(raw)
	if j.Name != raw {
		t.Errorf("setName(%q) stored %q, want it verbatim — sanitization belongs to Queue.SetName, which alone can read q.sOpts", raw, j.Name)
	}

	j.setScript("post.sh")
	if j.Script != "post.sh" {
		t.Errorf("setScript stored %q, want %q", j.Script, "post.sh")
	}

	j.setWarning("disk full")
	if j.Warning != "disk full" {
		t.Errorf("setWarning stored %q, want %q", j.Warning, "disk full")
	}

	j.setPar2ReleaseReason("volume 3 damaged")
	if got := j.progress.par2ReleaseReason; got != "volume 3 damaged" {
		t.Errorf("setPar2ReleaseReason stored %q, want %q", got, "volume 3 damaged")
	}
}

// TestJobMarkOnce_ReportsWhetherItTook pins the bool contract of
// markStartedOnce and markDownloadFinishedOnce, which their Queue wrappers
// use and which no other test observes.
//
// TestMarkJobStarted and TestMarkDownloadFinished assert the timestamp does
// not move on a second call — but a setter that correctly refuses the write
// and still returned true would pass both, while making the wrapper issue a
// redundant store.Update and mark the queue dirty on every repeat call. The
// timestamp is the effect; the bool is the interface.
func TestJobMarkOnce_ReportsWhetherItTook(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	t.Run("markStartedOnce", func(t *testing.T) {
		t.Parallel()
		j := &Job{ID: "j1", progress: &JobProgress{}}
		if !j.markStartedOnce(now) {
			t.Fatal("first call returned false, want true")
		}
		if j.markStartedOnce(later) {
			t.Error("second call returned true; the wrapper would persist and dirty the queue for a write that did not happen")
		}
		if !j.progress.downloadStarted.Equal(now) {
			t.Errorf("downloadStarted = %v, want %v", j.progress.downloadStarted, now)
		}
	})

	t.Run("markDownloadFinishedOnce", func(t *testing.T) {
		t.Parallel()
		j := &Job{ID: "j1", progress: &JobProgress{}}
		if !j.markDownloadFinishedOnce(now) {
			t.Fatal("first call returned false, want true")
		}
		if j.markDownloadFinishedOnce(later) {
			t.Error("second call returned true; the wrapper would persist and dirty the queue for a write that did not happen")
		}
		if !j.progress.downloadFinished.Equal(now) {
			t.Errorf("downloadFinished = %v, want %v", j.progress.downloadFinished, now)
		}
	})
}

// TestJobMarkOnce_RefusesAZeroTimestamp pins that a timestamp the store cannot
// distinguish from absent is refused rather than reported as a successful first
// mark. It began as #459's zero-value pin; #464 widened the rule from one value
// to an interval, and the subtests are the interval's boundary.
//
// The guard used to read the FIELD only. Handed time.Time{}, the field stayed
// zero and the method still returned true, so three things went wrong at once:
// the wrapper marked the queue dirty and ran store.Update for a write that did
// not happen, and — because the field was still zero — a later call with a real
// timestamp passed the guard again and overwrote, so "first wins" was violated
// and the store was written twice for one job start.
//
// What the methods cannot store is now every t whose Unix() is not positive:
// the zero value, because it is the sentinel their own first-wins test reads,
// and the rest of the interval because the SQLite column encodes it as the 0
// that the store decodes back to "absent". "half a second after the epoch" is
// the case that separates the two rules — it is not IsZero, so #459's guard
// admitted it, and it still encodes to 0.
//
// Neither is reachable from production today, but for different reasons, and
// conflating them is what an earlier draft of this comment did. markStartedOnce
// has one production caller — internal/app/pipeline.go:412, which passes
// time.Now(). markDownloadFinishedOnce has none at all; production reaches
// downloadFinished through Queue.SetPostProcStarted. Both are therefore
// hardening rather than a live defect, which is why the real assertion is the
// third one: that refusing does not consume the first-wins slot.
func TestJobMarkOnce_RefusesAZeroTimestamp(t *testing.T) {
	t.Parallel()

	real := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		mark  func(j *Job, t time.Time) bool
		field func(j *Job) time.Time
	}{
		{"markStartedOnce",
			func(j *Job, t time.Time) bool { return j.markStartedOnce(t) },
			func(j *Job) time.Time { return j.progress.downloadStarted }},
		{"markDownloadFinishedOnce",
			func(j *Job, t time.Time) bool { return j.markDownloadFinishedOnce(t) },
			func(j *Job) time.Time { return j.progress.downloadFinished }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, bad := range []struct {
				name string
				in   time.Time
			}{
				{"the zero value", time.Time{}},
				{"the epoch", time.Unix(0, 0)},
				{"half a second after the epoch", time.Unix(0, 500000000)},
			} {
				t.Run(bad.name, func(t *testing.T) {
					t.Parallel()
					// The fixture belongs inside this loop, not outside it:
					// shared across cases, the second one would start against
					// a job already holding real and Fatal below on a refusal
					// that is first-wins working correctly.
					j := &Job{ID: "j1", progress: &JobProgress{}}

					if tc.mark(j, bad.in) {
						t.Errorf("%s reported a successful mark; the wrapper would persist and dirty the queue for a write that did not happen", bad.name)
					}
					if got := tc.field(j); !got.IsZero() {
						t.Errorf("field = %v after a refused %s, want the zero time", got, bad.name)
					}
					// The refusal must not consume the first-wins slot: a real
					// timestamp arriving afterwards is still the FIRST mark.
					if !tc.mark(j, real) {
						t.Fatalf("a real timestamp after a refused %s returned false; the refusal consumed the first-wins slot", bad.name)
					}
					if got := tc.field(j); !got.Equal(real) {
						t.Errorf("field = %v, want %v", got, real)
					}
					// The other order: a bad value arriving AFTER a real mark
					// must not clobber it. A guard that stored t before
					// returning false — `if !isJobStamp(t) { j.progress.f = t;
					// return false }` — refuses correctly on the empty job
					// above and still destroys the timestamp here, so the
					// assertions above cannot see it.
					if tc.mark(j, bad.in) {
						t.Errorf("%s after a real one reported a successful mark", bad.name)
					}
					if got := tc.field(j); !got.Equal(real) {
						t.Errorf("field = %v after a refused %s clobbered it, want %v", got, bad.name, real)
					}
				})
			}
		})
	}
}

// TestJobRecordDownload_AccumulatesPerServer pins that recordDownload adds to
// the running total rather than replacing it, and initialises the map on
// first use.
//
// The lazy init is why the setter cannot be a one-line map write: a Job whose
// progress has never recorded a byte has a nil serverStats, and assigning
// into a nil map panics. Queue.RecordDownload is called once per completed
// article, so both the first call and the millionth run through here.
func TestJobRecordDownload_AccumulatesPerServer(t *testing.T) {
	t.Parallel()

	j := &Job{ID: "j1", progress: &JobProgress{}}
	if j.progress.serverStats != nil {
		t.Fatal("fixture already has serverStats; the nil-map path is what this test covers")
	}

	j.recordDownload("news.example.com", 100)
	j.recordDownload("news.example.com", 50)
	j.recordDownload("backup.example.com", 7)

	if got := j.progress.serverStats["news.example.com"]; got != 150 {
		t.Errorf("news.example.com = %d, want 150 — the second call must add, not replace", got)
	}
	if got := j.progress.serverStats["backup.example.com"]; got != 7 {
		t.Errorf("backup.example.com = %d, want 7", got)
	}
}

// TestJobDiscardDeferredPar2_OnlyTouchesIfNeeded pins which files the discard
// applies to, and that it reports no change when there is nothing to discard.
//
// Selectivity is the whole content of the method: FetchAlways files are being
// downloaded and must keep being downloaded, and FetchNever files are already
// discarded. Only FetchIfNeeded — a recovery volume still awaiting the CRC
// verdict — is the method's business. The false return matters because
// Queue.DiscardDeferredPar2 marks the queue dirty on it, and a method that
// always reported true would checkpoint on every no-op call.
func TestJobDiscardDeferredPar2_OnlyTouchesIfNeeded(t *testing.T) {
	t.Parallel()

	j := &Job{ID: "j1", progress: &JobProgress{files: []FileProgress{
		{Fetch: FetchAlways},
		{Fetch: FetchIfNeeded},
		{Fetch: FetchNever},
		{Fetch: FetchIfNeeded},
	}}}

	if !j.discardDeferredPar2() {
		t.Fatal("returned false with two FetchIfNeeded files, want true")
	}
	want := []FetchPolicy{FetchAlways, FetchNever, FetchNever, FetchNever}
	for i, w := range want {
		if got := j.progress.files[i].Fetch; got != w {
			t.Errorf("file %d policy = %v, want %v", i, got, w)
		}
	}

	if j.discardDeferredPar2() {
		t.Error("second call returned true with nothing left to discard; the wrapper would dirty the queue for a no-op")
	}
}

// TestJobResident covers all four combinations of the two pointer fields
// Job.resident tests, rather than only the two that occur in practice.
//
// The manifest-nil/progress-live row is the ordinary steady state for every
// queued or paused job, and the row that matters most: it must report
// ErrJobNotResident, because a manifest-tier caller that treated it as
// resident would dereference nil (#261). The both-nil and manifest-live/
// progress-nil rows are unreachable today — nothing leaves a job in q.byID
// with nil progress — and are asserted anyway, because that unreachability
// is a property of the callers and this gate must not depend on it.
func TestJobResident(t *testing.T) {
	t.Parallel()

	m := &Manifest{}
	p := &JobProgress{}

	for _, tc := range []struct {
		name     string
		manifest *Manifest
		progress *JobProgress
		want     error
	}{
		{"both present", m, p, nil},
		{"evicted: no manifest, progress stays", nil, p, ErrJobNotResident},
		{"no progress", m, nil, ErrJobNotResident},
		{"neither", nil, nil, ErrJobNotResident},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j := &Job{ID: "j1", manifest: tc.manifest, progress: tc.progress}
			err := j.resident()
			if !errors.Is(err, tc.want) {
				t.Fatalf("resident() = %v, want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "j1") {
				t.Errorf("resident() = %q, which names the job; the ID is the caller's to add "+
					"(Queue.residentJob wraps with the ID it was passed) and including it here double-names it", err)
			}
		})
	}
}
