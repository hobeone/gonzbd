package unpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	res, err := FileJoin(t.Context(), logger, archive, outDir, Options{})
	if err != nil {
		t.Fatalf("FileJoin: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("Result.Err: %v", res.Err)
	}

	logStr := logBuf.String()
	if !strings.Contains(logStr, `"part":1`) || !strings.Contains(logStr, `"pct":"33%"`) {
		t.Errorf("missing or incorrect progress logs for part 1 in: %s", logStr)
	}
	if !strings.Contains(logStr, `"part":2`) || !strings.Contains(logStr, `"pct":"67%"`) {
		t.Errorf("missing or incorrect progress logs for part 2 in: %s", logStr)
	}
	if !strings.Contains(logStr, `"part":3`) || !strings.Contains(logStr, `"pct":"100%"`) {
		t.Errorf("missing or incorrect progress logs for part 3 in: %s", logStr)
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
	_, err := FileJoin(t.Context(), slog.Default(), archive, t.TempDir(), Options{})
	if err == nil {
		t.Error("expected error for wrong archive type")
	}
}

func TestFileJoin_NoParts(t *testing.T) {
	t.Parallel()
	archive := Archive{Type: SplitArchive, Name: "empty", Parts: nil}
	_, err := FileJoin(t.Context(), slog.Default(), archive, t.TempDir(), Options{})
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

	res, err := FileJoin(t.Context(), slog.Default(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("expected no-op success when output exists, got error: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("expected no-op success, got Result.Err: %v", res.Err)
	}
	// Existing content should be preserved (not overwritten).
	got, _ := os.ReadFile(filepath.Join(outDir, "data"))
	if string(got) != "existing" {
		t.Errorf("existing file was modified: got %q, want %q", got, "existing")
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

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := FileJoin(ctx, slog.Default(), archive, outDir, Options{})
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

	_, err := FileJoin(t.Context(), slog.Default(), archive, t.TempDir(), Options{})
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

	_, err := FileJoin(t.Context(), slog.Default(), archive, outDir, Options{})
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

// TestScan_RecursiveSubdirectory verifies that Scan finds archives inside
// subdirectories. This is the common Usenet pattern where an NZB downloads
// files into a release-name subdirectory (e.g. release/movie.rar + movie.r00).
func TestScan_RecursiveSubdirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a release subdirectory with a legacy RAR set inside it.
	sub := filepath.Join(dir, "Release.Name-GROUP")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "movie.rar"), []byte("main"), 0o644)
	os.WriteFile(filepath.Join(sub, "movie.r00"), []byte("r00"), 0o644)
	os.WriteFile(filepath.Join(sub, "movie.r01"), []byte("r01"), 0o644)

	// Also a par2 file at top level (should be ignored by archive scan).
	os.WriteFile(filepath.Join(dir, "obfuscated.par2"), []byte("par2"), 0o644)

	archives, err := Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("expected 1 RAR set from subdirectory, got %d: %+v", len(archives), archives)
	}
	if archives[0].Type != RarArchive {
		t.Errorf("type = %d, want RarArchive", archives[0].Type)
	}
	if len(archives[0].Parts) != 3 {
		t.Errorf("parts = %d, want 3", len(archives[0].Parts))
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

func TestCopyPartDirect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("valid file", func(t *testing.T) {
		content := []byte("hello world")
		partPath := filepath.Join(dir, "valid.part")
		if err := os.WriteFile(partPath, content, 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		var buf strings.Builder
		err := copyPart(&buf, partPath)
		if err != nil {
			t.Fatalf("copyPart: %v", err)
		}

		if buf.String() != string(content) {
			t.Errorf("got %q, want %q", buf.String(), string(content))
		}
	})

	t.Run("empty file", func(t *testing.T) {
		partPath := filepath.Join(dir, "empty.part")
		if err := os.WriteFile(partPath, nil, 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}

		var buf strings.Builder
		err := copyPart(&buf, partPath)
		if err != nil {
			t.Fatalf("copyPart: %v", err)
		}

		if buf.Len() != 0 {
			t.Errorf("got length %d, want 0", buf.Len())
		}
	})

	t.Run("non-existent file", func(t *testing.T) {
		var buf strings.Builder
		err := copyPart(&buf, filepath.Join(dir, "does-not-exist"))
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})
}

func TestFileJoin_CopyError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outDir := t.TempDir()

	part1 := filepath.Join(dir, "movie.001")
	os.WriteFile(part1, []byte("AAAA"), 0o644)

	part2 := filepath.Join(dir, "movie.002")
	if err := os.Mkdir(part2, 0o755); err != nil {
		t.Fatal(err)
	}

	archive := Archive{
		Type:     SplitArchive,
		Name:     "movie",
		MainFile: part1,
		Parts:    []string{part1, part2},
	}

	_, err := FileJoin(t.Context(), slog.Default(), archive, outDir, Options{})
	if err == nil {
		t.Error("expected error when copying a directory part, but got nil")
	} else if !strings.HasPrefix(err.Error(), "filejoin: copy") {
		t.Errorf("expected copy error, got: %v", err)
	}
}

type midJoinInspectCtx struct {
	context.Context
	outDir          string
	finalName       string
	inspected       bool
	sawFinalMidJoin bool
	sawTempMidJoin  bool
	tempSizeMidJoin int64
}

func (c *midJoinInspectCtx) Err() error {
	if c.inspected {
		return context.Canceled
	}
	entries, err := os.ReadDir(c.outDir)
	if err != nil || len(entries) == 0 {
		return c.Context.Err()
	}
	// Wait until the first part has flushed bytes to disk inside outDir.
	var anyWritten bool
	for _, e := range entries {
		if info, infoErr := e.Info(); infoErr == nil && info.Size() > 0 {
			anyWritten = true
			break
		}
	}
	if !anyWritten {
		return c.Context.Err()
	}
	c.inspected = true
	if _, statErr := os.Stat(filepath.Join(c.outDir, c.finalName)); statErr == nil {
		c.sawFinalMidJoin = true
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gonzbd-tmp-") {
			c.sawTempMidJoin = true
			if info, infoErr := e.Info(); infoErr == nil {
				c.tempSizeMidJoin = info.Size()
			}
		}
	}
	return context.Canceled
}

// TestFileJoin_AtomicTempPublish verifies that FileJoin writes in-progress
// data to a .gonzbd-tmp-* sibling and only publishes the final name via rename
// after flush and close, so a mid-join interruption never leaves a truncated
// file under the final name.
func TestFileJoin_AtomicTempPublish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outDir := t.TempDir()

	// Size part 1 above joinBufSize so copyPart flushes bytes to disk before
	// FileJoin checks ctx.Err() ahead of part 2.
	part1Data := bytes.Repeat([]byte("A"), joinBufSize+64)
	part2Data := bytes.Repeat([]byte("B"), 64)
	part1 := filepath.Join(dir, "movie.001")
	part2 := filepath.Join(dir, "movie.002")
	if err := os.WriteFile(part1, part1Data, 0o644); err != nil {
		t.Fatalf("write part 1: %v", err)
	}
	if err := os.WriteFile(part2, part2Data, 0o644); err != nil {
		t.Fatalf("write part 2: %v", err)
	}

	archive := Archive{
		Type:     SplitArchive,
		Name:     "movie",
		MainFile: part1,
		Parts:    []string{part1, part2},
	}

	inspectCtx := &midJoinInspectCtx{
		Context:   t.Context(),
		outDir:    outDir,
		finalName: "movie",
	}
	if _, err := FileJoin(inspectCtx, slog.Default(), archive, outDir, Options{}); err == nil {
		t.Fatal("expected cancellation error mid-join, got nil")
	}
	if !inspectCtx.inspected {
		t.Fatal("fixture guard: midJoinInspectCtx never observed flushed bytes in outDir")
	}
	if inspectCtx.sawFinalMidJoin {
		t.Error("final output \"movie\" existed while join was in flight; want absent until atomic rename")
	}
	if !inspectCtx.sawTempMidJoin || inspectCtx.tempSizeMidJoin < joinBufSize {
		t.Errorf("sawTempMidJoin=%v tempSizeMidJoin=%d; want .gonzbd-tmp-* sibling with >= %d bytes mid-join",
			inspectCtx.sawTempMidJoin, inspectCtx.tempSizeMidJoin, joinBufSize)
	}

	// Deferred cleanup must have removed the unpublished temp file.
	if entries, err := os.ReadDir(outDir); err != nil || len(entries) != 0 {
		t.Errorf("outDir after cancelled join = %v (err=%v), want empty", entries, err)
	}

	// A subsequent run on the same outDir completes and leaves no temp file.
	res, err := FileJoin(t.Context(), slog.Default(), archive, outDir, Options{})
	if err != nil || res.Err != nil {
		t.Fatalf("rerun FileJoin: err=%v res.Err=%v", err, res.Err)
	}
	info, err := os.Stat(filepath.Join(outDir, "movie"))
	if err != nil {
		t.Fatalf("stat joined output: %v", err)
	}
	if want := int64(len(part1Data) + len(part2Data)); info.Size() != want {
		t.Errorf("joined size = %d, want %d", info.Size(), want)
	}

	// Joined output must have standard 0o666 &^ umask permissions, not 0o600.
	refPath := filepath.Join(dir, "umask_ref")
	refFile, err := os.OpenFile(refPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o666)
	if err != nil {
		t.Fatalf("open reference file: %v", err)
	}
	_ = refFile.Close()
	refInfo, err := os.Stat(refPath)
	if err != nil {
		t.Fatalf("stat reference file: %v", err)
	}
	if gotPerm, wantPerm := info.Mode().Perm(), refInfo.Mode().Perm(); gotPerm != wantPerm {
		t.Errorf("joined file mode = %04o, want %04o (0o666 &^ umask)", gotPerm, wantPerm)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "movie" {
		t.Errorf("outDir entries after complete join = %v, want only [movie]", entries)
	}
}

func TestFileJoin_OutputAlreadyExists_MissingFirstPart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outDir := t.TempDir()

	// Simulate a crash during archive cleanup after .001 was already unlinked
	// while .002 and .003 remain alongside the completed joined file.
	part2 := filepath.Join(dir, "data.002")
	part3 := filepath.Join(dir, "data.003")
	if err := os.WriteFile(part2, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part3, []byte("C"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "data"), []byte("ABC"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive := Archive{
		Type:     SplitArchive,
		Name:     "data",
		MainFile: part2,
		Parts:    []string{part2, part3},
	}

	res, err := FileJoin(t.Context(), slog.Default(), archive, outDir, Options{})
	if err != nil || res.Err != nil {
		t.Fatalf("expected no-op success when joined output exists despite missing .001, got err=%v res.Err=%v", err, res.Err)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ABC" {
		t.Errorf("existing file content = %q, want %q", got, "ABC")
	}
}

func TestFileJoin_OutputNonRegularNotTreatedAsExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outDir := t.TempDir()

	part1 := filepath.Join(dir, "data.001")
	part2 := filepath.Join(dir, "data.002")
	if err := os.WriteFile(part1, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part2, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plant a symlink at outRel so root.Lstat(outRel) succeeds with a
	// non-regular mode. FileJoin must not treat a non-regular entry as an
	// already-completed join output.
	if err := os.Symlink("missing-target", filepath.Join(outDir, "data")); err != nil {
		t.Fatal(err)
	}

	archive := Archive{
		Type:     SplitArchive,
		Name:     "data",
		MainFile: part1,
		Parts:    []string{part1, part2},
	}

	res, err := FileJoin(t.Context(), slog.Default(), archive, outDir, Options{})
	if err != nil || res.Err != nil {
		t.Fatalf("FileJoin with symlink at outRel failed: err=%v res.Err=%v", err, res.Err)
	}
	info, err := os.Lstat(filepath.Join(outDir, "data"))
	if err != nil {
		t.Fatalf("Lstat joined output: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("output mode = %v, want regular file replacing symlink", info.Mode())
	}
	got, err := os.ReadFile(filepath.Join(outDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "AB" {
		t.Errorf("joined content = %q, want %q", got, "AB")
	}
}

func TestSyncAndPublishJoin_SyncsBeforeCloseAndRename(t *testing.T) {
	t.Parallel()
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()

	const tmpRel = ".gonzbd-tmp-0123456789abcdef"
	if err := os.WriteFile(filepath.Join(outDir, tmpRel), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An os.Pipe write descriptor fails Sync() with EINVAL on Linux while
	// succeeding on Close(). If outFile.Sync() is skipped, Close() and
	// root.Rename() succeed and publish "movie" without an fsync.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	err = syncAndPublishJoin(w, root, tmpRel, "movie")
	if err == nil || !strings.Contains(err.Error(), "filejoin: sync output") {
		t.Fatalf("syncAndPublishJoin(pipe) = %v, want filejoin: sync output error", err)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "movie")); !os.IsNotExist(statErr) {
		t.Errorf("movie was published despite failed Sync() (stat err=%v)", statErr)
	}

	// Verify rename failure is surfaced when tmpRel is missing at publish time,
	// and that outFile was closed before Rename.
	f, err := os.CreateTemp(outDir, "sync-ok-*")
	if err != nil {
		t.Fatal(err)
	}
	err = syncAndPublishJoin(f, root, "missing-temp", "movie")
	if err == nil || !strings.Contains(err.Error(), "filejoin: publish output") {
		t.Fatalf("syncAndPublishJoin(missing temp) = %v, want filejoin: publish output error", err)
	}
	if closeErr := f.Close(); !errors.Is(closeErr, os.ErrClosed) {
		t.Errorf("outFile.Close() after syncAndPublishJoin = %v, want os.ErrClosed", closeErr)
	}
}
