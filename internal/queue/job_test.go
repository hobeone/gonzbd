package queue

import (
	"fmt"
	"testing"

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
	j := &Job{progress: &JobProgress{articlesResolved: 5, articlesFailed: 5}}
	if j.IsEarlyAbort() {
		t.Error("fired with only 5 resolved articles, need 10")
	}
}

func TestIsEarlyAbort_HighFailRate(t *testing.T) {
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 8}} // 80%
	if !j.IsEarlyAbort() {
		t.Error("should fire at 80% failure rate with 10 resolved")
	}
	if !j.progress.earlyAborted {
		t.Error("EarlyAborted flag should be set")
	}
}

func TestIsEarlyAbort_UnderThreshold(t *testing.T) {
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 7}} // 70%
	if j.IsEarlyAbort() {
		t.Error("should not fire at 70% failure rate")
	}
}

func TestIsEarlyAbort_OnlyFiresOnce(t *testing.T) {
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 10}}
	if !j.IsEarlyAbort() {
		t.Fatal("first call should fire")
	}
	if j.IsEarlyAbort() {
		t.Error("second call should not fire (already aborted)")
	}
}

func TestIsEarlyAbort_ExactThreshold(t *testing.T) {
	j := &Job{progress: &JobProgress{articlesResolved: 10, articlesFailed: 8}} // exactly 80%
	if !j.IsEarlyAbort() {
		t.Error("should fire at exactly 80%")
	}
}

func TestIsEarlyAbort_AllFailed(t *testing.T) {
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

// TestAdd_DoesNotBuildArtIndexEagerly pins the memory-reduction invariant: a
// freshly queued job that has not yet been touched by the download pipeline
// must not have allocated the messageID->index map. articleIndexByID builds
// it lazily on first access; Add's recompute call must not force it early.
func TestAdd_DoesNotBuildArtIndexEagerly(t *testing.T) {
	job, err := NewJob(minimalNZB(), AddOptions{Filename: "rel.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if job.manifest.messageIDIndex != nil {
		t.Fatalf("messageIDIndex was built eagerly at Add; want nil until first articleIndexByID call")
	}
}

// TestJob_DropArtIndex pins dropMessageIDIndex's own behavior directly on
// *Manifest, isolated from any Queue method that happens to call it.
func TestJob_DropArtIndex(t *testing.T) {
	job, err := NewJob(minimalNZB(), AddOptions{Filename: "rel.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	id := job.manifest.ArticleID(0)
	if _, ok := job.manifest.articleIndexByID(id); !ok {
		t.Fatal("articleIndexByID returned false for a present article")
	}
	if job.manifest.messageIDIndex == nil {
		t.Fatal("precondition: index should be built after articleIndexByID")
	}
	job.manifest.dropMessageIDIndex()
	if job.manifest.messageIDIndex != nil {
		t.Fatal("dropMessageIDIndex should nil out the index")
	}
	// articleIndexByID must still work afterward, rebuilding from scratch.
	idx, ok := job.manifest.articleIndexByID(id)
	if !ok || job.manifest.ArticleID(idx) != id {
		t.Fatalf("articleIndexByID(%q) after dropMessageIDIndex = (%v, %v), want a match", id, idx, ok)
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
	if err := q.MarkArticlesDone(job.ID, []string{"f0a0@t", "f0a1@t"}); err != nil {
		t.Fatalf("MarkArticlesDone(f0): %v", err)
	}
	if err := q.MarkFileComplete(job.ID, 0); err != nil {
		t.Fatalf("MarkFileComplete(0): %v", err)
	}
	// File 1: one succeeds, one fails.
	if err := q.MarkArticlesDone(job.ID, []string{"f1a0@t"}); err != nil {
		t.Fatalf("MarkArticlesDone(f1): %v", err)
	}
	if _, err := q.MarkArticlesFailed(job.ID, []string{"f1a1@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed(f1): %v", err)
	}
	// File 2: both articles fail.
	if _, err := q.MarkArticlesFailed(job.ID, []string{"f2a0@t", "f2a1@t"}); err != nil {
		t.Fatalf("MarkArticlesFailed(f2): %v", err)
	}

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
