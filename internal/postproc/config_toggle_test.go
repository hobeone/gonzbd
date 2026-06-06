package postproc

import (
	"os"
	"path/filepath"
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

	stage := &UnpackStage{
		EnableFileJoin:  false,
		EnableRecursive: false,
	}
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

// TestUnpackStage_EnableFileJoinTrue_JoinsSplitArchive verifies that
// when EnableFileJoin=true, split files are joined.
func TestUnpackStage_EnableFileJoinTrue_JoinsSplitArchive(t *testing.T) {
	t.Parallel()
	job, dir := stageJob(t)

	os.WriteFile(filepath.Join(dir, "data.001"), []byte("part1"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.002"), []byte("part2"), 0o644)

	stage := &UnpackStage{
		EnableFileJoin:  true,
		EnableRecursive: false,
	}
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

	stage := &UnpackStage{
		EnableFileJoin:  true,
		EnableRecursive: false,
	}
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

	stage := &UnpackStage{
		EnableFileJoin:  true,
		EnableRecursive: false,
		BaseOpts: unpack.Options{
			UnrarCommand:    "/nonexistent/unrar",
			SevenZipCommand: "/nonexistent/7z",
		},
	}
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
