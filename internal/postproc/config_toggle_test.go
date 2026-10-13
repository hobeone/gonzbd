package postproc

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// ---------- UnpackStage EnableFileJoin toggle ----------

// TestUnpackStage_EnableFileJoinFalse_SkipsSplitArchive verifies that
// when EnableFileJoin=false, split archives (.001/.002) are skipped.
func TestUnpackStage_EnableFileJoinFalse_SkipsSplitArchive(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)

	// Create split files that would normally be joined.
	os.WriteFile(filepath.Join(dir, "data.001"), []byte("part1"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.002"), []byte("part2"), 0o644)

	stage := appliedUnpackStage(UnpackConfig{
		EnableFileJoin:  false,
		EnableRecursive: false,
	})
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// No unpack error should occur (split was skipped, not failed).
	if job.UnpackError {
		t.Error("UnpackError should be false when file join is disabled")
	}
	// The joined output file should NOT exist.
	if _, err := os.Stat(filepath.Join(dir, "data")); err == nil {
		t.Error("joined output 'data' should NOT exist when EnableFileJoin=false")
	}
	// Original parts should still exist.
	if _, err := os.Stat(filepath.Join(dir, "data.001")); err != nil {
		t.Error("data.001 should still exist")
	}
}

// TestUnpackStage_DisabledArchiveType_IsDelivered verifies that an archive
// whose type is disabled (enable_unrar, enable_7zip, enable_filejoin or
// enable_tar off), with every other type enabled, is delivered untouched: it
// is not extracted or reported as extracted, its parts are not queued for
// deletion, the stage's permissions are not applied, and finalize moves it to
// FinalDir (#943, #947).
func TestUnpackStage_DisabledArchiveType_IsDelivered(t *testing.T) {
	t.Parallel()
	all := UnpackConfig{EnableUnrar: true, Enable7zip: true, EnableFileJoin: true, EnableTar: true}
	cases := []struct {
		name  string
		files []string
		// fixture, when set, is the one file's content: a real archive, so
		// an extraction that should not happen would succeed.
		fixture  string
		disable  func(*UnpackConfig)
		mainBase string
		// product is a file the archive extracts to, absent when skipped.
		product string
	}{
		{name: "split with filejoin off", files: []string{"data.001", "data.002"}, disable: func(c *UnpackConfig) { c.EnableFileJoin = false }, mainBase: "data", product: "data"},
		{name: "tar with tar off", files: []string{"x.tar"}, disable: func(c *UnpackConfig) { c.EnableTar = false }, mainBase: "x"},
		{name: "rar with unrar off", files: []string{"release.rar"}, fixture: par2LayoutFixture("layout_b", "release.rar"), disable: func(c *UnpackConfig) { c.EnableUnrar = false }, mainBase: "release", product: "feature.bin"},
		{name: "7z with 7zip off", files: []string{"release.7z"}, fixture: par2LayoutFixture("layout_b", "release.7z"), disable: func(c *UnpackConfig) { c.Enable7zip = false }, mainBase: "release", product: "feature.bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job, dir := stageJob(t)
			job.FinalDir = filepath.Join(t.TempDir(), "final")
			want := make(map[string][]byte, len(tc.files))
			for _, f := range tc.files {
				want[f] = []byte("payload " + f)
				if tc.fixture != "" {
					b, err := os.ReadFile(tc.fixture)
					if err != nil {
						t.Fatal(err)
					}
					want[f] = b
				}
				p := filepath.Join(dir, f)
				if err := os.WriteFile(p, want[f], 0o644); err != nil {
					t.Fatal(err)
				}
				// WriteFile's mode is masked by the umask; the mode
				// assertion below needs exactly 0644.
				if err := os.Chmod(p, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cfg := all
			cfg.Base = unpack.Options{UseGoRAR: true, UseGo7z: true}
			cfg.EnableRecursive = true
			cfg.Cleanup = true
			cfg.Permissions = "700"
			tc.disable(&cfg)
			stage := appliedUnpackStage(cfg)
			stage.Log = slog.New(slog.DiscardHandler)
			if err := stage.Run(t.Context(), job); err != nil {
				t.Fatalf("unpack Run: %v", err)
			}
			if job.UnpackError {
				t.Error("UnpackError = true; a disabled archive type is skipped, not failed")
			}
			if len(job.pendingDeletions) != 0 {
				t.Errorf("pendingDeletions = %v; want none for a skipped archive", job.pendingDeletions)
			}
			if tc.product != "" {
				if _, err := os.Lstat(filepath.Join(dir, tc.product)); err == nil {
					t.Errorf("%s exists; a disabled archive type must not be extracted", tc.product)
				}
			}
			for _, line := range job.OutputLines {
				if strings.Contains(line, "Extracted "+tc.mainBase+" successfully") {
					t.Errorf("stage log reports a skipped archive as extracted: %q", line)
				}
				// Run's second scan logs "(pass 2/3)", then "(pass 2)" on
				// its result; a skip extracts nothing, so no second scan.
				if strings.Contains(line, "(pass 2") {
					t.Errorf("a skipped archive triggered a second scan: %q", line)
				}
			}
			info, err := os.Stat(filepath.Join(dir, tc.files[0]))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o644 {
				t.Errorf("%s mode = %o; want 644 (permissions apply only after an extraction)", tc.files[0], got)
			}

			fin := NewFinalizeStage()
			fin.Log = slog.New(slog.DiscardHandler)
			if err := fin.Run(t.Context(), job); err != nil {
				t.Fatalf("finalize Run: %v", err)
			}
			for _, f := range tc.files {
				got, err := os.ReadFile(filepath.Join(job.FinalDir, f))
				if err != nil {
					t.Errorf("%s not delivered to FinalDir: %v", f, err)
					continue
				}
				if !bytes.Equal(got, want[f]) {
					t.Errorf("%s content differs from what was delivered (%d bytes, want %d)", f, len(got), len(want[f]))
				}
			}
		})
	}
}

// TestUnpackStage_EnableFileJoinTrue_JoinsSplitArchive verifies that
// when EnableFileJoin=true, split files are joined.
func TestUnpackStage_EnableFileJoinTrue_JoinsSplitArchive(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)

	os.WriteFile(filepath.Join(dir, "data.001"), []byte("part1"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.002"), []byte("part2"), 0o644)

	stage := appliedUnpackStage(UnpackConfig{
		EnableFileJoin:  true,
		EnableRecursive: false,
	})
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.UnpackError {
		t.Error("UnpackError should be false for successful join")
	}
	// The joined output file SHOULD exist.
	data, err := os.ReadFile(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("read joined output: %v", err)
	}
	if string(data) != "part1part2" {
		t.Errorf("joined content = %q, want %q", data, "part1part2")
	}
}

// ---------- UnpackStage EnableRecursive toggle ----------

// TestUnpackStage_EnableRecursiveFalse_SinglePass verifies that
// when EnableRecursive=false, only one pass runs.
func TestUnpackStage_EnableRecursiveFalse_SinglePass(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)

	// Create split files.
	os.WriteFile(filepath.Join(dir, "data.001"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.002"), []byte("world"), 0o644)

	stage := appliedUnpackStage(UnpackConfig{
		EnableFileJoin:  true,
		EnableRecursive: false,
	})
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One pass should join the files successfully.
	data, err := os.ReadFile(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("read joined output: %v", err)
	}
	if string(data) != "helloworld" {
		t.Errorf("joined content = %q, want %q", data, "helloworld")
	}
}

// ---------- UnpackStage archive type ordering (I5) ----------

// TestUnpackStage_JoinBeforeExtract verifies that split files are
// joined before RAR/7z extraction is attempted (I5: join before unpack
// ordering). We create split files that should be joined first; RAR files
// with non-existent binaries should be skipped/failed after join succeeds.
func TestUnpackStage_JoinBeforeExtract(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)

	// Create split files that should be joined.
	os.WriteFile(filepath.Join(dir, "video.001"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "video.002"), []byte("B"), 0o644)

	// Create a fake RAR file (will fail because unrar binary doesn't exist,
	// but the join should still succeed first due to ordering).
	os.WriteFile(filepath.Join(dir, "archive.part01.rar"), []byte("not-a-rar"), 0o644)

	stage := appliedUnpackStage(UnpackConfig{
		EnableFileJoin:  true,
		EnableRecursive: false,
		Base: unpack.Options{
			UnrarCommand:    "/nonexistent/unrar",
			SevenZipCommand: "/nonexistent/7z",
		},
	})
	// Will have an error due to RAR, but join should succeed.
	_ = stage.Run(t.Context(), job)

	// The join should have succeeded regardless of RAR failure.
	data, err := os.ReadFile(filepath.Join(dir, "video"))
	if err != nil {
		t.Fatalf("join should have succeeded: %v", err)
	}
	if string(data) != "AB" {
		t.Errorf("joined content = %q, want %q", data, "AB")
	}
}

// ---------- archiveTypePriority ordering ----------

// TestArchiveTypePriority verifies the ordering: splits < rar < 7z < unknown.
func TestArchiveTypePriority(t *testing.T) {
	t.Parallel()
	split := archiveTypePriority(unpack.SplitArchive)
	rar := archiveTypePriority(unpack.RarArchive)
	seven := archiveTypePriority(unpack.SevenZipArchive)
	unknown := archiveTypePriority(unpack.UnknownArchive)

	if split >= rar {
		t.Errorf("SplitArchive priority (%d) should be < RarArchive (%d)", split, rar)
	}
	if rar >= seven {
		t.Errorf("RarArchive priority (%d) should be < SevenZipArchive (%d)", rar, seven)
	}
	if seven >= unknown {
		t.Errorf("SevenZipArchive priority (%d) should be < Unknown (%d)", seven, unknown)
	}
}
