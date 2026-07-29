package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// multiVolumeNZB returns a minimal non-trivial (MNT) parsed NZB containing
// multiple RAR data volumes and a PAR2 recovery volume across multiple articles.
func multiVolumeNZB() *nzb.NZB {
	return &nzb.NZB{
		Files: []nzb.File{
			{
				Subject: "movie.part01.rar",
				Bytes:   200,
				Articles: []nzb.Article{
					{ID: "r1.1@b", Bytes: 100, Number: 1},
					{ID: "r1.2@b", Bytes: 100, Number: 2},
				},
			},
			{
				Subject: "movie.part02.rar",
				Bytes:   200,
				Articles: []nzb.Article{
					{ID: "r2.1@b", Bytes: 100, Number: 1},
					{ID: "r2.2@b", Bytes: 100, Number: 2},
				},
			},
			{
				Subject: "movie.vol00+01.par2",
				Bytes:   100,
				Articles: []nzb.Article{
					{ID: "p1.1@b", Bytes: 100, Number: 1},
				},
			},
		},
	}
}

func TestBuildIngestJob_HappyPath(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}

	job, err := BuildIngestJob(cfg, multiVolumeNZB(), "movie.nzb", types.FetchOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if job.Filename != "movie.nzb" {
		t.Errorf("Filename = %q, want %q", job.Filename, "movie.nzb")
	}
	if job.Manifest().NumFiles() != 3 {
		t.Errorf("NumFiles = %d, want 3", job.Manifest().NumFiles())
	}
	if job.Manifest().TotalBytes() != 500 {
		t.Errorf("TotalBytes = %d, want 500", job.Manifest().TotalBytes())
	}
	if job.Manifest().Par2Bytes() != 100 {
		t.Errorf("Par2Bytes = %d, want 100", job.Manifest().Par2Bytes())
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

	job, err := BuildIngestJob(cfg, multiVolumeNZB(), "movie.nzb", types.FetchOptions{
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

	job, err := BuildIngestJob(cfg, multiVolumeNZB(), "my cool release.nzb", types.FetchOptions{}, nil)
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

	job, err := BuildIngestJob(nil, multiVolumeNZB(), "movie.nzb", types.FetchOptions{PP: types.PPInherit}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if job.PP != 3 {
		t.Errorf("PP = %d, want 3 (builtin default category fallback)", job.PP)
	}
}

func TestBuildIngestJob_PausedPriority(t *testing.T) {
	t.Parallel()

	job, err := BuildIngestJob(nil, multiVolumeNZB(), "movie.nzb", types.FetchOptions{
		Priority: constants.PausedPriority,
	}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if job.Priority != constants.PausedPriority {
		t.Errorf("Priority = %v, want %v", job.Priority, constants.PausedPriority)
	}
	if job.Status != constants.StatusPaused {
		t.Errorf("Status = %v, want %v", job.Status, constants.StatusPaused)
	}
}
