package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
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

func TestBuildIngestJob_HappyPath(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}

	job, err := BuildIngestJob(cfg, minimalNZB(), "test.nzb", types.FetchOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if job.Filename != "test.nzb" {
		t.Errorf("Filename = %q, want %q", job.Filename, "test.nzb")
	}
	if job.Manifest().NumFiles() != 1 {
		t.Errorf("NumFiles = %d, want 1", job.Manifest().NumFiles())
	}
}

// TestBuildIngestJob_CategoryPriorityInherit pins the bug fixed by this
// consolidation: the one-shot CLI path previously passed a nil Categories
// slice, so a custom Default category's Priority (and PP/Script) were
// silently ignored in favor of the builtin fallback. Passing live config
// through BuildIngestJob must resolve them.
func TestBuildIngestJob_CategoryPriorityInherit(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Categories: []config.CategoryConfig{
			{Name: "Default", PP: 1, Script: "custom.sh", Priority: int(constants.HighPriority)},
		},
	}

	job, err := BuildIngestJob(cfg, minimalNZB(), "test.nzb", types.FetchOptions{
		PP:       types.PPInherit,
		Priority: constants.DefaultPriority,
	}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if job.PP != 1 {
		t.Errorf("PP = %d, want 1 (from configured Default category)", job.PP)
	}
	if job.Script != "custom.sh" {
		t.Errorf("Script = %q, want %q (from configured Default category)", job.Script, "custom.sh")
	}
	if job.Priority != constants.HighPriority {
		t.Errorf("Priority = %d, want %d (from configured Default category)", job.Priority, constants.HighPriority)
	}
}

// TestBuildIngestJob_SanitizeOptionsApplied pins the second half of the
// one-shot bug fix: sanitize-related config (e.g. ReplaceSpacesWith) must
// affect the derived job name, not just categories.
func TestBuildIngestJob_SanitizeOptionsApplied(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Downloads: config.DownloadConfig{
			ReplaceSpacesWith: ".",
		},
	}

	job, err := BuildIngestJob(cfg, minimalNZB(), "my cool release.nzb", types.FetchOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if job.Name != "my.cool.release" {
		t.Errorf("Name = %q, want %q (spaces replaced per config)", job.Name, "my.cool.release")
	}
}

// TestBuildIngestJob_NilConfig confirms the defensive nil-config fallback
// (kept for consistency with existing nil-guard patterns at call sites)
// still resolves a sane default rather than panicking.
func TestBuildIngestJob_NilConfig(t *testing.T) {
	t.Parallel()

	job, err := BuildIngestJob(nil, minimalNZB(), "test.nzb", types.FetchOptions{PP: types.PPInherit}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if job.PP != 3 {
		t.Errorf("PP = %d, want 3 (builtin default category fallback)", job.PP)
	}
}
