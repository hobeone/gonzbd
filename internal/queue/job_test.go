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
	j := &Job{ArticlesResolved: 5, ArticlesFailed: 5}
	if j.IsEarlyAbort() {
		t.Error("fired with only 5 resolved articles, need 10")
	}
}

func TestIsEarlyAbort_HighFailRate(t *testing.T) {
	j := &Job{ArticlesResolved: 10, ArticlesFailed: 8} // 80%
	if !j.IsEarlyAbort() {
		t.Error("should fire at 80% failure rate with 10 resolved")
	}
	if !j.EarlyAborted {
		t.Error("EarlyAborted flag should be set")
	}
}

func TestIsEarlyAbort_UnderThreshold(t *testing.T) {
	j := &Job{ArticlesResolved: 10, ArticlesFailed: 7} // 70%
	if j.IsEarlyAbort() {
		t.Error("should not fire at 70% failure rate")
	}
}

func TestIsEarlyAbort_OnlyFiresOnce(t *testing.T) {
	j := &Job{ArticlesResolved: 10, ArticlesFailed: 10}
	if !j.IsEarlyAbort() {
		t.Fatal("first call should fire")
	}
	if j.IsEarlyAbort() {
		t.Error("second call should not fire (already aborted)")
	}
}

func TestIsEarlyAbort_ExactThreshold(t *testing.T) {
	j := &Job{ArticlesResolved: 10, ArticlesFailed: 8} // exactly 80%
	if !j.IsEarlyAbort() {
		t.Error("should fire at exactly 80%")
	}
}

func TestIsEarlyAbort_AllFailed(t *testing.T) {
	j := &Job{ArticlesResolved: 10, ArticlesFailed: 10} // 100%
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
	j := &Job{
		Files: []JobFile{
			{Articles: []JobArticle{
				{ID: "a1", Bytes: 100, Done: true},               // resolved, succeeded
				{ID: "a2", Bytes: 100, Done: true, Failed: true}, // resolved, failed
				{ID: "a3", Bytes: 100, Done: true, Failed: true}, // resolved, failed
				{ID: "a4", Bytes: 100},                           // still pending
			}},
		},
	}
	// Simulate the state right after a JSON unmarshal from disk: the
	// json:"-" transient counters are zero even though the persisted
	// article flags above record 3 already-resolved articles (2 failed).
	j.ArticlesResolved = 0
	j.ArticlesFailed = 0

	j.recomputePending()

	if j.ArticlesResolved != 3 {
		t.Errorf("ArticlesResolved = %d, want 3 (count of Done articles)", j.ArticlesResolved)
	}
	if j.ArticlesFailed != 2 {
		t.Errorf("ArticlesFailed = %d, want 2 (count of Done&&Failed articles)", j.ArticlesFailed)
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
		art := JobArticle{ID: fmt.Sprintf("a%d", i), Bytes: 100, Done: true}
		if i < 8 {
			art.Failed = true
		}
		articles = append(articles, art)
	}
	j := &Job{Files: []JobFile{{Articles: articles}}}
	// Simulate post-unmarshal zeroing of the transient counters.
	j.ArticlesResolved = 0
	j.ArticlesFailed = 0

	j.recomputePending()

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
// must not have allocated the messageID->*JobArticle index. articleByID
// builds it lazily on first access; recomputePending must not force it early.
func TestAdd_DoesNotBuildArtIndexEagerly(t *testing.T) {
	job, err := NewJob(minimalNZB(), AddOptions{Filename: "rel.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	q := New()
	if err := q.Add(job); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if job.artIdx != nil {
		t.Fatalf("artIdx was built eagerly at Add; want nil until first articleByID call")
	}
	// FileIdx back-pointers must still be correct without the map, since
	// recomputePending's own loop sets them independently of buildArtIndex.
	if job.Files[0].Articles[0].FileIdx != 0 {
		t.Fatalf("FileIdx = %d, want 0", job.Files[0].Articles[0].FileIdx)
	}
}

// TestJob_DropArtIndex pins dropArtIndex's own behavior directly on *Job,
// isolated from any Queue method that happens to call it.
func TestJob_DropArtIndex(t *testing.T) {
	job, err := NewJob(minimalNZB(), AddOptions{Filename: "rel.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	id := job.Files[0].Articles[0].ID
	if got := job.articleByID(id); got == nil {
		t.Fatal("articleByID returned nil for a present article")
	}
	if job.artIdx == nil {
		t.Fatal("precondition: index should be built after articleByID")
	}
	job.dropArtIndex()
	if job.artIdx != nil {
		t.Fatal("dropArtIndex should nil out the index")
	}
	// articleByID must still work afterward, rebuilding from scratch.
	if got := job.articleByID(id); got == nil || got.ID != id {
		t.Fatalf("articleByID(%q) after dropArtIndex = %v, want a match", id, got)
	}
}
