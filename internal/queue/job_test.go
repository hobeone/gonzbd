package queue

import (
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
		{Name: "tv", PP: 7, Script: "tv.sh", Priority: int(constants.HighPriority)},
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
	if job.PP != 7 {
		t.Errorf("PP = %d, want 7 (from category)", job.PP)
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
		{Name: "tv", PP: 7, Script: "tv.sh", Priority: int(constants.LowPriority)},
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

func TestNewJob_NoCategoriesFallback(t *testing.T) {
	// No Categories: sentinels should clamp to safe defaults.
	job, err := NewJob(minimalNZB(), AddOptions{
		Filename: "test.nzb",
		PP:       types.PPInherit,
		Priority: constants.DefaultPriority,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.PP != 0 {
		t.Errorf("PP = %d, want 0 (clamped default)", job.PP)
	}
	if job.Priority != constants.NormalPriority {
		t.Errorf("Priority = %d, want %d (clamped default)", job.Priority, constants.NormalPriority)
	}
}

func TestNewJob_CategoryFallbackToDefault(t *testing.T) {
	cats := []config.CategoryConfig{
		{Name: "Default", PP: 5, Script: "fallback.sh"},
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
	if job.PP != 5 {
		t.Errorf("PP = %d, want 5 (from Default category)", job.PP)
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
