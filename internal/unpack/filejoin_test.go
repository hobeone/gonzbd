package unpack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// ---------- FileJoin ----------

func TestFileJoin_Success(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outDir := t.TempDir()

	// Create 3 split parts.
	for i, content := range []string{"AAAA", "BBBB", "CCCC"} {
		path := filepath.Join(dir, "movie."+partSuffix(i+1))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write part %d: %v", i, err)
		}
	}

	archive := Archive{
		Type:     SplitArchive,
		Name:     "movie",
		MainFile: filepath.Join(dir, "movie.001"),
		Parts: []string{
			filepath.Join(dir, "movie.001"),
			filepath.Join(dir, "movie.002"),
			filepath.Join(dir, "movie.003"),
		},
	}

	res, err := FileJoin(context.Background(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("FileJoin: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("Result.Err: %v", res.Err)
	}

	joined, err := os.ReadFile(filepath.Join(outDir, "movie"))
	if err != nil {
		t.Fatalf("read joined: %v", err)
	}
	if string(joined) != "AAAABBBBCCCC" {
		t.Errorf("joined content = %q, want %q", joined, "AAAABBBBCCCC")
	}
}

func TestFileJoin_WrongArchiveType(t *testing.T) {
	t.Parallel()
	archive := Archive{Type: RarArchive, Name: "wrong"}
	_, err := FileJoin(context.Background(), archive, t.TempDir(), Options{})
	if err == nil {
		t.Error("expected error for wrong archive type")
	}
}

func TestFileJoin_NoParts(t *testing.T) {
	t.Parallel()
	archive := Archive{Type: SplitArchive, Name: "empty", Parts: nil}
	_, err := FileJoin(context.Background(), archive, t.TempDir(), Options{})
	if err == nil {
		t.Error("expected error for no parts")
	}
}

func TestFileJoin_OutputAlreadyExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outDir := t.TempDir()

	partPath := filepath.Join(dir, "data.001")
	os.WriteFile(partPath, []byte("X"), 0o644)
	// Create the output file first.
	os.WriteFile(filepath.Join(outDir, "data"), []byte("existing"), 0o644)

	archive := Archive{
		Type:     SplitArchive,
		Name:     "data",
		MainFile: partPath,
		Parts:    []string{partPath},
	}

	_, err := FileJoin(context.Background(), archive, outDir, Options{})
	if err == nil {
		t.Error("expected error when output file already exists")
	}
}

func TestFileJoin_ContextCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outDir := t.TempDir()

	// Create 2 parts.
	for i := 1; i <= 2; i++ {
		os.WriteFile(filepath.Join(dir, "cancel."+partSuffix(i)), []byte("data"), 0o644)
	}

	archive := Archive{
		Type:     SplitArchive,
		Name:     "cancel",
		MainFile: filepath.Join(dir, "cancel.001"),
		Parts:    []string{filepath.Join(dir, "cancel.001"), filepath.Join(dir, "cancel.002")},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := FileJoin(ctx, archive, outDir, Options{})
	if err == nil {
		t.Error("expected error for cancelled context")
	}

	// Output file should be cleaned up.
	if _, err := os.Stat(filepath.Join(outDir, "cancel")); !os.IsNotExist(err) {
		t.Error("partial output should be cleaned up on cancellation")
	}
}

func TestFileJoin_NonContiguousParts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create parts .001 and .003 (missing .002).
	os.WriteFile(filepath.Join(dir, "gap.001"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "gap.003"), []byte("C"), 0o644)

	archive := Archive{
		Type:     SplitArchive,
		Name:     "gap",
		MainFile: filepath.Join(dir, "gap.001"),
		Parts:    []string{filepath.Join(dir, "gap.001"), filepath.Join(dir, "gap.003")},
	}

	_, err := FileJoin(context.Background(), archive, t.TempDir(), Options{})
	if err == nil {
		t.Error("expected error for non-contiguous parts")
	}
}

func TestFileJoin_MissingPartFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outDir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "miss.001"), []byte("A"), 0o644)
	// .002 does not exist on disk.

	archive := Archive{
		Type:     SplitArchive,
		Name:     "miss",
		MainFile: filepath.Join(dir, "miss.001"),
		Parts:    []string{filepath.Join(dir, "miss.001"), filepath.Join(dir, "miss.002")},
	}

	_, err := FileJoin(context.Background(), archive, outDir, Options{})
	if err == nil {
		t.Error("expected error for missing part file")
	}

	// Partial output should be cleaned up.
	if _, err := os.Stat(filepath.Join(outDir, "miss")); !os.IsNotExist(err) {
		t.Error("partial output should be cleaned up")
	}
}

// ---------- Scan ----------

func TestScan_MixedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create files of various types.
	os.WriteFile(filepath.Join(dir, "movie.part01.rar"), []byte("rar1"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.part02.rar"), []byte("rar2"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.001"), []byte("split1"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.002"), []byte("split2"), 0o644)
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("text"), 0o644)
	os.WriteFile(filepath.Join(dir, "archive.7z"), []byte("7z"), 0o644)

	archives, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Should find: 1 RAR set, 1 split set, 1 7z single.
	if len(archives) != 3 {
		t.Errorf("found %d archives, want 3: %+v", len(archives), archives)
	}

	typeCount := make(map[ArchiveType]int)
	for _, a := range archives {
		typeCount[a.Type]++
	}
	if typeCount[RarArchive] != 1 {
		t.Errorf("RAR archives = %d, want 1", typeCount[RarArchive])
	}
	if typeCount[SplitArchive] != 1 {
		t.Errorf("Split archives = %d, want 1", typeCount[SplitArchive])
	}
	if typeCount[SevenZipArchive] != 1 {
		t.Errorf("7z archives = %d, want 1", typeCount[SevenZipArchive])
	}
}

func TestScan_NonexistentDir(t *testing.T) {
	t.Parallel()
	_, err := Scan("/nonexistent/directory")
	if err == nil {
		t.Error("expected error for nonexistent dir")
	}
}

func TestScan_SkipsDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "subdir.rar"), 0o755) // dir named like archive

	archives, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(archives) != 0 {
		t.Errorf("expected 0 archives, got %d", len(archives))
	}
}

func TestScan_LegacyRAR(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "movie.rar"), []byte("main"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.r00"), []byte("r00"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.r01"), []byte("r01"), 0o644)

	archives, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("expected 1 legacy RAR set, got %d", len(archives))
	}
	if archives[0].Type != RarArchive {
		t.Errorf("type = %d, want RarArchive", archives[0].Type)
	}
	if len(archives[0].Parts) != 3 {
		t.Errorf("parts = %d, want 3", len(archives[0].Parts))
	}
}

func TestScan_SevenZipSplitVolumes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "backup.7z.001"), []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(dir, "backup.7z.002"), []byte("v2"), 0o644)
	// Also a single .7z — should be suppressed by the split set.
	os.WriteFile(filepath.Join(dir, "backup.7z"), []byte("single"), 0o644)

	archives, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("expected 1 archive (split suppresses single), got %d", len(archives))
	}
	if archives[0].Type != SevenZipArchive {
		t.Errorf("type = %d, want SevenZipArchive", archives[0].Type)
	}
	if len(archives[0].Parts) != 2 {
		t.Errorf("parts = %d, want 2 (split volumes)", len(archives[0].Parts))
	}
}

// ---------- sortedNumericParts ----------

func TestSortedNumericParts_ValidSequence(t *testing.T) {
	t.Parallel()
	parts := []string{"/d/x.003", "/d/x.001", "/d/x.002"}
	sorted, err := sortedNumericParts(parts)
	if err != nil {
		t.Fatalf("sortedNumericParts: %v", err)
	}
	if len(sorted) != 3 {
		t.Errorf("len = %d, want 3", len(sorted))
	}
	// Verify sorted order: .001, .002, .003.
	for i, p := range sorted {
		want := fmt.Sprintf("/d/x.%03d", i+1)
		if p != want {
			t.Errorf("sorted[%d] = %q, want %q", i, p, want)
		}
	}
}

func TestSortedNumericParts_Gap(t *testing.T) {
	t.Parallel()
	parts := []string{"/d/x.001", "/d/x.003"} // missing .002
	_, err := sortedNumericParts(parts)
	if err == nil {
		t.Error("expected error for gap in sequence")
	}
}

func TestSortedNumericParts_NoSuffix(t *testing.T) {
	t.Parallel()
	parts := []string{"/d/readme.txt"}
	_, err := sortedNumericParts(parts)
	if err == nil {
		t.Error("expected error for non-numeric suffix")
	}
}

// partSuffix returns "001", "002", etc.
func partSuffix(n int) string {
	return fmt.Sprintf("%03d", n)
}
