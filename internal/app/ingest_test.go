package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/constants"
	"github.com/hobeone/gonzbd/internal/job"
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
				Subject: "movie.vol01+02.par2",
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

	j, hdr, err := BuildIngestJob(cfg, multiVolumeNZB(), "movie.nzb", types.FetchOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if hdr.Filename != "movie.nzb" {
		t.Errorf("Filename = %q, want %q", hdr.Filename, "movie.nzb")
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.NumFiles() != 3 {
		t.Errorf("NumFiles = %d, want 3", m.NumFiles())
	}
	if m.TotalBytes() != 500 {
		t.Errorf("TotalBytes = %d, want 500", m.TotalBytes())
	}
	if m.RecoveryBytes() != 100 {
		t.Errorf("Par2Bytes = %d, want 100", m.RecoveryBytes())
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

	_, hdr, err := BuildIngestJob(cfg, multiVolumeNZB(), "movie.nzb", types.FetchOptions{
		PP:       types.PPInherit,
		Priority: constants.DefaultPriority,
	}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if hdr.PP != 1 {
		t.Errorf("PP = %d, want 1 (from configured Default category)", hdr.PP)
	}
	if hdr.Script != "custom.sh" {
		t.Errorf("Script = %q, want %q (from configured Default category)", hdr.Script, "custom.sh")
	}
	if hdr.Priority != int(constants.HighPriority) {
		t.Errorf("Priority = %d, want %d (from configured Default category)", hdr.Priority, constants.HighPriority)
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

	j, _, err := BuildIngestJob(cfg, multiVolumeNZB(), "my cool release.nzb", types.FetchOptions{}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if j.Name() != "my.cool.release" {
		t.Errorf("Name = %q, want %q (spaces replaced per config)", j.Name(), "my.cool.release")
	}
}

// TestBuildIngestJob_NilConfig confirms the defensive nil-config fallback
// (kept for consistency with existing nil-guard patterns at call sites)
// still resolves a sane default rather than panicking.
func TestBuildIngestJob_NilConfig(t *testing.T) {
	t.Parallel()

	_, hdr, err := BuildIngestJob(nil, multiVolumeNZB(), "movie.nzb", types.FetchOptions{PP: types.PPInherit}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if hdr.PP != 3 {
		t.Errorf("PP = %d, want 3 (builtin default category fallback)", hdr.PP)
	}
}

func TestBuildIngestJob_PausedPriority(t *testing.T) {
	t.Parallel()

	j, hdr, err := BuildIngestJob(nil, multiVolumeNZB(), "movie.nzb", types.FetchOptions{
		Priority: constants.PausedPriority,
	}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if hdr.Priority != int(constants.PausedPriority) {
		t.Errorf("Priority = %v, want %v", hdr.Priority, constants.PausedPriority)
	}
	if j.Intent() != job.IntentPause {
		t.Errorf("Intent = %v, want %v", j.Intent(), job.IntentPause)
	}
}

func TestDeriveName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input, want string
	}{
		{"/path/to/movie.nzb", "movie"},
		{"/path/to/archive.tar.gz", "archive.tar"},
		{"/path/to/archive.tar", "archive"},
		{"barefile", "barefile"},
	}
	for _, tc := range cases {
		if got := deriveName(tc.input); got != tc.want {
			t.Errorf("deriveName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
