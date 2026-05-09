package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- MoveFile ----------

func TestMoveFile_SameDevice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	content := []byte("hello, move me")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := MoveFile(src, dst); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}

	// Source should be gone.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source should be removed after move, err = %v", err)
	}

	// Destination should have the correct content.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("dst content = %q, want %q", got, content)
	}
}

func TestMoveFile_PreservesPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "perms.txt")
	dst := filepath.Join(dir, "perms_moved.txt")

	if err := os.WriteFile(src, []byte("restricted"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := MoveFile(src, dst); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat dst: %v", err)
	}
	// On same device, os.Rename preserves mode. The mode bits should
	// at least include the owner read/write we set.
	perm := info.Mode().Perm()
	if perm&0o600 != 0o600 {
		t.Errorf("permissions = %o, want at least 0600", perm)
	}
}

func TestMoveFile_SourceNotExist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := MoveFile(filepath.Join(dir, "nonexistent"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("expected error for nonexistent source")
	}
}

func TestMoveFile_DstDirNotExist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := MoveFile(src, filepath.Join(dir, "nodir", "dst.txt"))
	if err == nil {
		t.Error("expected error when destination directory doesn't exist")
	}
}

func TestMoveFile_OverwritesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile dst: %v", err)
	}

	if err := MoveFile(src, dst); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != "new" {
		t.Errorf("dst content = %q, want %q", got, "new")
	}
}

func TestMoveFile_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.txt")
	dst := filepath.Join(dir, "empty_moved.txt")

	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := MoveFile(src, dst); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size = %d, want 0", info.Size())
	}
}

// ---------- IsCrossDeviceError / IsRenameMergeNeeded ----------

func TestIsCrossDeviceError(t *testing.T) {
	t.Parallel()
	// A normal error should not be cross-device.
	if IsCrossDeviceError(os.ErrNotExist) {
		t.Error("ErrNotExist should not be cross-device")
	}
	if IsCrossDeviceError(nil) {
		t.Error("nil should not be cross-device")
	}
}

func TestIsRenameMergeNeeded(t *testing.T) {
	t.Parallel()
	if IsRenameMergeNeeded(os.ErrNotExist) {
		t.Error("ErrNotExist should not need merge")
	}
	if IsRenameMergeNeeded(nil) {
		t.Error("nil should not need merge")
	}
}

// ---------- GetUniqueFilename ----------

func TestGetUniqueFilename_NoConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "unique.txt")
	got := GetUniqueFilename(path)
	if got != path {
		t.Errorf("GetUniqueFilename = %q, want %q (no conflict)", got, path)
	}
}

func TestGetUniqueFilename_OneConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("exists"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := GetUniqueFilename(path)
	want := filepath.Join(dir, "file.1.txt")
	if got != want {
		t.Errorf("GetUniqueFilename = %q, want %q", got, want)
	}
}

func TestGetUniqueFilename_MultipleConflicts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "report.txt")

	// Create original + .1 + .2
	for _, name := range []string{"report.txt", "report.1.txt", "report.2.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("exists"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	got := GetUniqueFilename(base)
	want := filepath.Join(dir, "report.3.txt")
	if got != want {
		t.Errorf("GetUniqueFilename = %q, want %q", got, want)
	}
}

func TestGetUniqueFilename_NoExtension(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "readme")
	if err := os.WriteFile(path, []byte("exists"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got := GetUniqueFilename(path)
	want := filepath.Join(dir, "readme.1")
	if got != want {
		t.Errorf("GetUniqueFilename = %q, want %q", got, want)
	}
}

// ---------- JoinSafe ----------

func TestJoinSafe_Normal(t *testing.T) {
	t.Parallel()
	got := JoinSafe("/downloads", "my-show", "file.rar", SanitizeOptions{})
	want := filepath.Join("/downloads", "my-show", "file.rar")
	if got != want {
		t.Errorf("JoinSafe = %q, want %q", got, want)
	}
}

func TestJoinSafe_EmptyFolderAndFile(t *testing.T) {
	t.Parallel()
	got := JoinSafe("/downloads", "", "", SanitizeOptions{})
	if got != "/downloads" {
		t.Errorf("JoinSafe = %q, want %q", got, "/downloads")
	}
}

func TestJoinSafe_OnlyFolder(t *testing.T) {
	t.Parallel()
	got := JoinSafe("/downloads", "myfolder", "", SanitizeOptions{})
	want := filepath.Join("/downloads", "myfolder")
	if got != want {
		t.Errorf("JoinSafe = %q, want %q", got, want)
	}
}

func TestJoinSafe_OnlyFile(t *testing.T) {
	t.Parallel()
	got := JoinSafe("/downloads", "", "file.txt", SanitizeOptions{})
	want := filepath.Join("/downloads", "file.txt")
	if got != want {
		t.Errorf("JoinSafe = %q, want %q", got, want)
	}
}

func TestJoinSafe_TruncatesLongFolder(t *testing.T) {
	t.Parallel()
	base := "/dl"
	longFolder := strings.Repeat("A", 300)
	got := JoinSafe(base, longFolder, "f.txt", SanitizeOptions{})
	// Folder component must fit within NAME_MAX (255 bytes).
	folderComponent := filepath.Base(filepath.Dir(got))
	if len(folderComponent) > maxFilenameBytes {
		t.Errorf("folder component length %d exceeds max %d", len(folderComponent), maxFilenameBytes)
	}
	if !strings.HasSuffix(got, "f.txt") {
		t.Errorf("path should end with filename, got %q", got)
	}
}

func TestJoinSafe_TruncatesLongFile(t *testing.T) {
	t.Parallel()
	base := "/dl"
	longFile := strings.Repeat("B", 300) + ".rar"
	got := JoinSafe(base, "folder", longFile, SanitizeOptions{})
	// File component must fit within NAME_MAX (255 bytes).
	fileComponent := filepath.Base(got)
	if len(fileComponent) > maxFilenameBytes {
		t.Errorf("file component length %d exceeds max %d", len(fileComponent), maxFilenameBytes)
	}
	if !strings.HasSuffix(got, ".rar") {
		t.Errorf("file extension should be preserved, got %q", got)
	}
}

// TestJoinSafe_FolderStableAcrossFiles verifies that the folder component
// is identical regardless of filename length — files in the same job must
// land in the same directory. Regression test for a bug where per-call
// path-budget arithmetic produced a different truncated folder per file.
func TestJoinSafe_FolderStableAcrossFiles(t *testing.T) {
	t.Parallel()
	base := "/dl"
	folder := "Daemons.of.the.Shadow.Realm.S01E06.The.Kagemori.Clan.and.the.Unknown.Assailants.1080p.NF.WEB-DL.JPN.AAC2.0.H.264.MSubs-ToonsHub"
	shortFile := "x.par2"
	longFile := folder + ".part01.rar"

	gotShort := JoinSafe(base, folder, shortFile, SanitizeOptions{})
	gotLong := JoinSafe(base, folder, longFile, SanitizeOptions{})

	if filepath.Dir(gotShort) != filepath.Dir(gotLong) {
		t.Errorf("folder differs between files:\n  short: %s\n  long:  %s",
			filepath.Dir(gotShort), filepath.Dir(gotLong))
	}
}

func TestJoinSafe_SanitizesIllegalChars(t *testing.T) {
	t.Parallel()
	got := JoinSafe("/dl", "my:folder", "my*file.txt", SanitizeOptions{})
	// ':' and '*' should be replaced by the sanitizer.
	if strings.ContainsAny(got, ":*") {
		t.Errorf("path should not contain illegal chars: %q", got)
	}
}
