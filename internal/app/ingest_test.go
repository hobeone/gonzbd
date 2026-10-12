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

// TestBuildIngestJob_PolicyMatchesPolicyFromPP pins that the Policy
// BuildIngestJob attaches to the job must equal job.PolicyFromPP(pp)
// for the resolved PP, field for field — including Verify, which a prior
// version of this function left at its zero value (false) regardless of pp.
// A caller such as runAssess that started gating the on-demand par2 fetch
// decision on Policy.Verify or Policy.Repair would otherwise see Verify
// false even for a PP>=1 job.
func TestBuildIngestJob_PolicyMatchesPolicyFromPP(t *testing.T) {
	t.Parallel()
	for _, pp := range []int{types.PPNone, types.PPRepair, types.PPUnpack, types.PPDelete} {
		j, _, err := BuildIngestJob(&config.Config{}, multiVolumeNZB(), "movie.nzb",
			types.FetchOptions{PP: pp}, nil)
		if err != nil {
			t.Fatalf("BuildIngestJob(pp=%d): %v", pp, err)
		}
		want := job.PolicyFromPP(pp)
		if got := j.Policy(); got != want {
			t.Errorf("BuildIngestJob(pp=%d).Policy() = %+v, want %+v (job.PolicyFromPP(%d))", pp, got, want, pp)
		}
	}
}

// TestBuildIngestJob_ClampsAnOutOfRangePP pins docs/sabnzbd_spec.md §8.2's
// rule for an add's pp: a level outside [0,3] is saturated, so pp=7 runs as 3
// and pp=-5 as 0, in the header and in the job's Policy alike.
func TestBuildIngestJob_ClampsAnOutOfRangePP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want int }{{7, 3}, {4, 3}, {-5, 0}} {
		j, hdr, err := BuildIngestJob(&config.Config{}, multiVolumeNZB(), "movie.nzb",
			types.FetchOptions{PP: tc.in}, nil)
		if err != nil {
			t.Fatalf("BuildIngestJob(pp=%d): %v", tc.in, err)
		}
		if hdr.PP.Int() != tc.want || j.Policy() != job.PolicyFromPP(tc.want) {
			t.Errorf("BuildIngestJob(pp=%d) = header PP %d, Policy %+v; want %d, %+v",
				tc.in, hdr.PP.Int(), j.Policy(), tc.want, job.PolicyFromPP(tc.want))
		}
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
	if hdr.PP.Int() != 1 {
		t.Errorf("PP = %d, want 1 (from configured Default category)", hdr.PP.Int())
	}
	if hdr.Script != "custom.sh" {
		t.Errorf("Script = %q, want %q (from configured Default category)", hdr.Script, "custom.sh")
	}
	if hdr.Priority != constants.HighPriority {
		t.Errorf("Priority = %d, want %d (from configured Default category)", hdr.Priority, constants.HighPriority)
	}
}

// TestBuildIngestJob_CategoryDefaultPriorityResolves pins that no sentinel
// priority reaches the dispatcher from ingest. A category whose priority is
// DefaultPriority ("use the global default") inherits the fallback
// category's, and Normal when that is a sentinel too. Stored raw, -100 would
// sort the job behind Low in dispatch order and the queue listing.
func TestBuildIngestJob_CategoryDefaultPriorityResolves(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		fallback constants.Priority
		want     constants.Priority
	}{
		{"fallback category is concrete", constants.HighPriority, constants.HighPriority},
		{"fallback category is Default too", constants.DefaultPriority, constants.NormalPriority},
		{"fallback category is Stop", constants.StopPriority, constants.NormalPriority},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Categories: []config.CategoryConfig{
					{Name: "Default", PP: 1, Priority: int(tc.fallback)},
					{Name: "tv", PP: 1, Priority: int(constants.DefaultPriority)},
				},
			}
			_, hdr, err := BuildIngestJob(cfg, multiVolumeNZB(), "movie.nzb", types.FetchOptions{
				Category: "tv",
				PP:       types.PPInherit,
				Priority: constants.DefaultPriority,
			}, nil)
			if err != nil {
				t.Fatalf("BuildIngestJob: %v", err)
			}
			if hdr.Priority != tc.want {
				t.Errorf("Priority = %d, want %d", hdr.Priority, tc.want)
			}
		})
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
	if hdr.PP.Int() != 3 {
		t.Errorf("PP = %d, want 3 (builtin default category fallback)", hdr.PP.Int())
	}
}

// TestBuildIngestJob_RefusesNonConcretePriority pins that ingest refuses a
// request ResolvePriority cannot turn into a concrete priority, so no ingest
// source can store Stop or an undefined value in a header.
func TestBuildIngestJob_RefusesNonConcretePriority(t *testing.T) {
	t.Parallel()
	for _, p := range []constants.Priority{constants.StopPriority, 7} {
		t.Run(p.String(), func(t *testing.T) {
			t.Parallel()
			_, hdr, err := BuildIngestJob(&config.Config{}, multiVolumeNZB(), "movie.nzb", types.FetchOptions{Priority: p}, nil)
			if err == nil {
				t.Fatalf("BuildIngestJob(priority %s) = header priority %d, nil; want an error", p, hdr.Priority)
			}
		})
	}
}

// TestBuildIngestJob_PausedPriority pins that a Paused request pauses the job
// and gives it the priority its category resolves to, not the -2 sentinel,
// which would sort it behind Low and keep it there after a resume.
func TestBuildIngestJob_PausedPriority(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Categories: []config.CategoryConfig{
		{Name: "Default", PP: 3, Priority: int(constants.LowPriority)},
		{Name: "tv", PP: 3, Priority: int(constants.HighPriority)},
	}}
	j, hdr, err := BuildIngestJob(cfg, multiVolumeNZB(), "movie.nzb", types.FetchOptions{
		Category: "tv",
		Priority: constants.PausedPriority,
	}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if hdr.Priority != constants.HighPriority {
		t.Errorf("Priority = %v, want %v (tv's)", hdr.Priority, constants.HighPriority)
	}
	if j.Intent() != job.IntentPause {
		t.Errorf("Intent = %v, want %v", j.Intent(), job.IntentPause)
	}
}

// TestBuildIngestJob_FilenameCompressionSuffixStripped pins the fix for the
// double-.gz backup bug: dirscanner and urlgrabber both hand BuildIngestJob
// the on-disk filename of the (now-decompressed) source, which for a
// gzip/bz2-compressed watch-folder drop still carries the compression
// suffix (e.g. "movie.nzb.gz"). hdr.Filename feeds writeNZBBackup, which
// always gzips the raw bytes for the admin/nzb/ backup and unconditionally
// appends ".gz" — so an unstripped Filename produced "movie.nzb.gz.gz".
// Mirrors SABnzbd's process_single_nzb, which strips the same suffix at the
// equivalent convergence point (nzbparser.py: filename.replace(".nzb.gz", ".nzb")).
func TestBuildIngestJob_FilenameCompressionSuffixStripped(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cases := []struct {
		input, want string
	}{
		{"movie.nzb.gz", "movie.nzb"},
		{"movie.nzb.bz2", "movie.nzb"},
		{"movie.nzb", "movie.nzb"},
		{"bundle.zip", "bundle.zip"},
	}
	for _, tc := range cases {
		_, hdr, err := BuildIngestJob(cfg, multiVolumeNZB(), tc.input, types.FetchOptions{}, nil)
		if err != nil {
			t.Fatalf("BuildIngestJob(%q): %v", tc.input, err)
		}
		if hdr.Filename != tc.want {
			t.Errorf("BuildIngestJob(%q) Filename = %q, want %q", tc.input, hdr.Filename, tc.want)
		}
	}
}

func TestStripCompressionSuffix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input, want string
	}{
		{"movie.nzb.gz", "movie.nzb"},
		{"movie.nzb.bz2", "movie.nzb"},
		{"MOVIE.NZB.GZ", "MOVIE.NZB"},
		{"movie.nzb", "movie.nzb"},
		{"bundle.zip", "bundle.zip"},
		{"archive.tar.gz", "archive.tar.gz"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripCompressionSuffix(tc.input); got != tc.want {
			t.Errorf("stripCompressionSuffix(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestStripNZBExt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input, want string
	}{
		{"movie.nzb", "movie"},
		{"movie.nzb.gz", "movie"},
		{"movie.nzb.bz2", "movie"},
		{"MOVIE.NZB.GZ", "MOVIE"},
		{"bundle.zip", "bundle.zip"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripNZBExt(tc.input); got != tc.want {
			t.Errorf("stripNZBExt(%q) = %q, want %q", tc.input, got, tc.want)
		}
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
