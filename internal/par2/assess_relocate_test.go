package par2

import (
	"crypto/md5"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestAssess_BasenameMatch(t *testing.T) {
	// Setup: create a download dir with a flat file and a par2 file whose
	// manifest references the file in a subdirectory.
	dir := t.TempDir()

	// Create a flat file "screenshot.jpg" with known content.
	content := []byte("fake screenshot content for testing")
	if err := os.WriteFile(filepath.Join(dir, "screenshot.jpg"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate par2 manifest that says the file should be at "Screens/screenshot.jpg".
	manifest := []FileDesc{
		{
			FileName: "Screens/screenshot.jpg",
			FileSize: uint64(len(content)),
			Hash16k:  md5.Sum(content), // content < 16KB, hash covers all
		},
	}

	// We can't use real par2 files, so call the internal matching directly.
	// Instead, test via the exported QuickCheck with a mock Set.
	// Since QuickCheck calls ParseFileDescriptions which needs a real par2 file,
	// we test the relocation logic through a helper approach.

	// Create a minimal test by directly calling relocateFile.
	ok := relocateFile(dir, "screenshot.jpg", manifest[0], nil)
	if !ok {
		t.Fatal("relocateFile returned false")
	}

	// Verify: file should now be at Screens/screenshot.jpg.
	dest := filepath.Join(dir, "Screens", "screenshot.jpg")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file not found at expected path %s: %v", dest, err)
	}

	// Verify: original flat file should be gone.
	src := filepath.Join(dir, "screenshot.jpg")
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("original file should be gone, but stat returned: %v", err)
	}
}

func TestAssess_FlattenedNameMatch(t *testing.T) {
	// When SanitizeFilename converts "Screens/foo.jpg" to "Screens_foo.jpg",
	// the quick-check should match it back to the par2 entry.
	dir := t.TempDir()

	content := []byte("flattened file content")
	if err := os.WriteFile(filepath.Join(dir, "Screens_foo.jpg"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	fd := FileDesc{
		FileName: "Screens/foo.jpg",
		FileSize: uint64(len(content)),
	}

	// The basename "foo.jpg" doesn't match "Screens_foo.jpg", so phase 1 skips.
	// But the flattened name "Screens_foo.jpg" matches, so phase 2 should catch it.

	// Phase 1: basename doesn't match.
	basename := filepath.Base(fd.FileName) // "foo.jpg"
	if _, err := os.Stat(filepath.Join(dir, basename)); !os.IsNotExist(err) {
		t.Fatal("expected basename not to exist for this test")
	}

	// Phase 2: flattened name matches — test relocateFile directly.
	ok := relocateFile(dir, "Screens_foo.jpg", fd, nil)
	if !ok {
		t.Fatal("relocateFile returned false for flattened name")
	}

	// Verify relocation.
	dest := filepath.Join(dir, "Screens", "foo.jpg")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file not found at expected path: %v", err)
	}
}

func TestAssess_Hash16kMatch(t *testing.T) {
	dir := t.TempDir()

	// Create an obfuscated file with random-looking name.
	content := make([]byte, 32*1024) // 32KB so hash16k covers first 16KB
	for i := range content {
		content[i] = byte(i % 256)
	}
	if err := os.WriteFile(filepath.Join(dir, "a1b2c3d4.dat"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Compute hash16k (MD5 of first 16KB).
	want := md5.Sum(content[:16*1024])

	fd := FileDesc{
		FileName: "Extras/real_name.dat",
		FileSize: uint64(len(content)),
		Hash16k:  want,
	}

	// Test ComputeHash16k.
	got, err := ComputeHash16k(filepath.Join(dir, "a1b2c3d4.dat"))
	if err != nil {
		t.Fatalf("ComputeHash16k: %v", err)
	}
	if got != want {
		t.Fatalf("hash16k mismatch: got %x, want %x", got, want)
	}

	// Test relocateFile with the hash-matched entry.
	ok := relocateFile(dir, "a1b2c3d4.dat", fd, nil)
	if !ok {
		t.Fatal("relocateFile returned false for hash16k match")
	}

	dest := filepath.Join(dir, "Extras", "real_name.dat")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file not found at expected path: %v", err)
	}
}

func TestAssess_PathTraversal(t *testing.T) {
	dir := t.TempDir()

	content := []byte("should not escape")
	if err := os.WriteFile(filepath.Join(dir, "evil.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	fd := FileDesc{
		FileName: "../../etc/evil.txt",
		FileSize: uint64(len(content)),
	}

	ok := relocateFile(dir, "evil.txt", fd, nil)
	if ok {
		t.Fatal("relocateFile should reject path traversal")
	}

	// Original file should still be in place.
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err != nil {
		t.Fatalf("original file should still exist: %v", err)
	}
}

func TestAssess_SizeMismatch(t *testing.T) {
	dir := t.TempDir()

	content := []byte("short")
	if err := os.WriteFile(filepath.Join(dir, "file.dat"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	fd := FileDesc{
		FileName: "Sub/file.dat",
		FileSize: 99999, // doesn't match actual size
	}

	ok := relocateFile(dir, "file.dat", fd, nil)
	if ok {
		t.Fatal("relocateFile should skip on size mismatch")
	}

	// File should remain in place.
	if _, err := os.Stat(filepath.Join(dir, "file.dat")); err != nil {
		t.Fatalf("file should still exist: %v", err)
	}
}

func TestAssess_NoSubdirs(t *testing.T) {
	dir := t.TempDir()

	content := []byte("flat file")
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// A flat par2 set naming exactly what is on disk. QuickCheck now
	// IDENTIFIES these — it used to refuse to look at a flat set at all — so
	// the thing that keeps it from churning the filesystem is NeedsRename,
	// not the absence of matching. Renaming here would be a self-move.
	//
	// This test previously asserted nothing: its only branch called t.Log,
	// and it never invoked QuickCheck. It is the regression that matters for
	// the gate's removal, so it now runs the real thing.
	sets := par2SetFor(t, dir, map[string][]byte{"movie.mkv": content})

	renames, err := assessAndApply(t, dir, sets, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("QuickCheck: %v", err)
	}
	if len(renames) != 0 {
		t.Errorf("QuickCheck relocated %+v for a correctly-named flat file; expected none", renames)
	}
	if _, err := os.Stat(filepath.Join(dir, "movie.mkv")); err != nil {
		t.Errorf("movie.mkv is no longer where it was: %v", err)
	}
}

func TestComputeHash16k_SmallFile(t *testing.T) {
	dir := t.TempDir()

	// File smaller than 16KB — hash covers entire content.
	content := []byte("tiny file")
	path := filepath.Join(dir, "tiny.txt")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ComputeHash16k(path)
	if err != nil {
		t.Fatalf("ComputeHash16k: %v", err)
	}

	want := md5.Sum(content)
	if got != want {
		t.Fatalf("hash mismatch for small file: got %x, want %x", got, want)
	}
}

func TestComputeHash16k_LargeFile(t *testing.T) {
	dir := t.TempDir()

	// File larger than 16KB — hash covers only first 16KB.
	content := make([]byte, 32*1024)
	for i := range content {
		content[i] = byte(i % 251)
	}
	path := filepath.Join(dir, "large.bin")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ComputeHash16k(path)
	if err != nil {
		t.Fatalf("ComputeHash16k: %v", err)
	}

	want := md5.Sum(content[:16*1024])
	if got != want {
		t.Fatalf("hash mismatch for large file: got %x, want %x", got, want)
	}
}

func TestRelocateFile_CreatesNestedDirs(t *testing.T) {
	dir := t.TempDir()

	content := []byte("deeply nested")
	if err := os.WriteFile(filepath.Join(dir, "pic.jpg"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	fd := FileDesc{
		FileName: "a/b/c/pic.jpg",
		FileSize: uint64(len(content)),
	}

	ok := relocateFile(dir, "pic.jpg", fd, nil)
	if !ok {
		t.Fatal("relocateFile returned false for nested dirs")
	}

	dest := filepath.Join(dir, "a", "b", "c", "pic.jpg")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file not at nested path: %v", err)
	}
}

func TestComputeFileCRC32(t *testing.T) {
	dir := t.TempDir()

	// Known CRC32 of "hello world\n" = 0x888b2612 (NOT the value for
	// "hello world" without newline — use an explicit check).
	content := []byte("test data for CRC32")
	path := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := computeFileCRC32(path)
	if err != nil {
		t.Fatalf("computeFileCRC32: %v", err)
	}

	// Compute expected CRC32 using the same algorithm.
	h := crc32.NewIEEE()
	h.Write(content)
	want := h.Sum32()

	if got != want {
		t.Errorf("CRC32 mismatch: got %08x, want %08x", got, want)
	}
}

func TestComputeFileCRC32_MissingFile(t *testing.T) {
	_, err := computeFileCRC32("/nonexistent/file")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestQuickCheck_Phase4_CRCSizeFallback tests that Phase 4 (CRC32+Size)
// correctly relocates an obfuscated file that didn't match in phases 1-3.
func TestAssess_Phase4_CRCSizeFallback(t *testing.T) {
	dir := t.TempDir()

	// Create an obfuscated file with content that has a known CRC32.
	content := make([]byte, 1024)
	for i := range content {
		content[i] = byte(i % 137)
	}
	obfuscatedName := "deadbeef1234.dat"
	if err := os.WriteFile(filepath.Join(dir, obfuscatedName), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Compute its CRC32.
	h := crc32.NewIEEE()
	h.Write(content)
	expectedCRC := h.Sum32()

	// Create a FileDesc with a different basename (won't match phase 1),
	// different flattened name (won't match phase 2), and a unique Hash16k
	// that doesn't match the file (won't match phase 3), but matching
	// CRC32+size (will match phase 4).
	fd := FileDesc{
		FileName:  "Subs/real_name.dat",
		FileSize:  uint64(len(content)),
		Hash16k:   [16]byte{0xff}, // won't match anything
		FileCRC32: expectedCRC,
	}

	ok := relocateFile(dir, obfuscatedName, fd, nil)
	if !ok {
		t.Fatal("relocateFile should succeed for CRC+size match")
	}

	dest := filepath.Join(dir, "Subs", "real_name.dat")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file not found at expected path %s: %v", dest, err)
	}

	if _, err := os.Stat(filepath.Join(dir, obfuscatedName)); !os.IsNotExist(err) {
		t.Fatal("original file should be gone after relocation")
	}
}

// TestQuickCheck_Phase3HashMatch_EndToEnd drives QuickCheck itself (not just
// relocateFile/ComputeHash16k in isolation) through Phase 3's hash16k index
// build-and-match loop: the obfuscated flat file matches neither the par2
// basename nor the flattened name, so only the hash16k comparison can find it.
func TestAssess_Phase3HashMatch_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	parPath := filepath.Join(dir, "test.par2")

	setID := [16]byte{0x10}
	fileID := [16]byte{0x11}
	fileName := "Subdir/realname.dat"

	content := make([]byte, 32*1024) // > 16KB, so hash16k covers a true prefix
	for i := range content {
		content[i] = byte(i % 251)
	}
	hash16k := md5.Sum(content[:16*1024])

	// No IFSC packet, so FileCRC32 stays 0 and Phase 4 cannot claim this
	// entry — only the Phase 3 hash16k path can match it.
	fdBody := buildFileDescBody(fileID, [16]byte{}, hash16k, uint64(len(content)), fileName)
	pkt := buildPacket(setID, typeFileDesc, fdBody)
	if err := os.WriteFile(parPath, pkt, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Name matches neither "realname.dat" (basename) nor "Subdir_realname.dat"
	// (flattened) — phases 1 and 2 must both miss.
	obfuscatedName := "a1b2c3d4e5f6.bin"
	if err := os.WriteFile(filepath.Join(dir, obfuscatedName), content, 0o644); err != nil {
		t.Fatal(err)
	}

	sets := []Set{{Name: "test", MainFile: parPath}}
	renames, err := assessAndApply(t, dir, sets, nil)
	if err != nil {
		t.Fatalf("QuickCheck: %v", err)
	}

	if len(renames) != 1 {
		t.Fatalf("got %d renames, want 1 (phase 3 hash16k match)", len(renames))
	}
	if renames[0].From != obfuscatedName || renames[0].To != fileName {
		t.Errorf("rename = %+v, want From=%q To=%q", renames[0], obfuscatedName, fileName)
	}
	if _, err := os.Stat(filepath.Join(dir, "Subdir", "realname.dat")); err != nil {
		t.Errorf("file not relocated to expected path: %v", err)
	}
}

// TestQuickCheck_Phase4CRCMatch_EndToEnd drives QuickCheck itself through
// Phase 4's CRC32+size index build-and-match loop. The entry's Hash16k is
// deliberately wrong (so Phase 3 misses it) but its FileCRC32 — reconstructed
// from a single full IFSC slice via Combine(0, crc, n) == crc — matches the
// flat file's actual CRC32 and size, so only Phase 4 can find it.
func TestAssess_Phase4CRCMatch_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	parPath := filepath.Join(dir, "test.par2")

	setID := [16]byte{0x20}
	fileID := [16]byte{0x21}
	fileName := "Subdir/other.dat"

	content := make([]byte, 4096)
	for i := range content {
		content[i] = byte(i % 137)
	}
	fileSize := uint64(len(content))
	actualCRC := crc32.ChecksumIEEE(content)
	if actualCRC == 0 {
		t.Fatal("test content CRC32 is zero — pick different content so the phase 4 filter (FileCRC32 > 0) applies")
	}

	// sliceSize == fileSize ⇒ exactly one full slice with zero tail padding,
	// so reconstructCRCs combines it as Combine(0, crc, sliceSize) == crc.
	mainPkt := buildPacket(setID, typeMain, buildMainBody(fileSize, fileID))
	fdBody := buildFileDescBody(fileID, [16]byte{}, [16]byte{0xDE, 0xAD}, fileSize, fileName)
	fdPkt := buildPacket(setID, typeFileDesc, fdBody)
	ifscBody := buildIFSCBody(fileID, []ifscSlice{{crc32: actualCRC}})
	ifscPkt := buildPacket(setID, typeIFSC, ifscBody)

	buf := make([]byte, 0, len(mainPkt)+len(fdPkt)+len(ifscPkt))
	buf = append(buf, mainPkt...)
	buf = append(buf, fdPkt...)
	buf = append(buf, ifscPkt...)
	if err := os.WriteFile(parPath, buf, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Name matches neither "other.dat" (basename) nor "Subdir_other.dat"
	// (flattened), and its hash16k won't match {0xDE, 0xAD, ...} — only the
	// CRC32+size fallback can find it.
	obfuscatedName := "f00dbabe9988.dat"
	if err := os.WriteFile(filepath.Join(dir, obfuscatedName), content, 0o644); err != nil {
		t.Fatal(err)
	}

	sets := []Set{{Name: "test", MainFile: parPath}}
	renames, err := assessAndApply(t, dir, sets, nil)
	if err != nil {
		t.Fatalf("QuickCheck: %v", err)
	}

	if len(renames) != 1 {
		t.Fatalf("got %d renames, want 1 (phase 4 CRC32+size match)", len(renames))
	}
	if renames[0].From != obfuscatedName || renames[0].To != fileName {
		t.Errorf("rename = %+v, want From=%q To=%q", renames[0], obfuscatedName, fileName)
	}
	if _, err := os.Stat(filepath.Join(dir, "Subdir", "other.dat")); err != nil {
		t.Errorf("file not relocated to expected path: %v", err)
	}
}

func TestAssess_Integration(t *testing.T) {
	tmpDir := t.TempDir()
	parPath := filepath.Join(tmpDir, "test.par2")

	setID := [16]byte{1, 2, 3, 4}
	fileID := [16]byte{10, 20, 30}
	fullHash := [16]byte{0xAA, 0xBB, 0xCC}

	content := []byte("flat file content for quickcheck integration test")
	hash16k := md5.Sum(content)
	fileName := "Subdir/original.txt"

	// Build the par2 file.
	body := buildFileDescBody(fileID, fullHash, hash16k, uint64(len(content)), fileName)
	pkt := buildPacket(setID, typeFileDesc, body)
	if err := os.WriteFile(parPath, pkt, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create the flat file matching the par2 expected file.
	// Use flattened name: "Subdir_original.txt" (since slash turns to underscore).
	flatName := "Subdir_original.txt"
	if err := os.WriteFile(filepath.Join(tmpDir, flatName), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Call QuickCheck
	sets := []Set{
		{
			Name:     "test",
			MainFile: parPath,
		},
	}
	renames, err := assessAndApply(t, tmpDir, sets, nil)
	if err != nil {
		t.Fatalf("QuickCheck failed: %v", err)
	}

	if len(renames) != 1 {
		t.Fatalf("expected 1 rename, got %d", len(renames))
	}
	if renames[0].From != flatName || renames[0].To != "Subdir/original.txt" {
		t.Errorf("unexpected rename: %+v", renames[0])
	}

	// Verify file is relocated.
	dest := filepath.Join(tmpDir, "Subdir", "original.txt")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file not found at relocated path %s: %v", dest, err)
	}
}

func TestAssess_EdgeCases(t *testing.T) {
	// Case 1: Empty manifests list (or no par2 main file)
	t.Run("empty main file skips set", func(t *testing.T) {
		sets := []Set{
			{Name: "empty"},
		}
		renames, err := assessAndApply(t, t.TempDir(), sets, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected 0 renames, got %d", len(renames))
		}
	})

	// Case 2: Parse file failure (does not exist)
	t.Run("parse failure warns and continues", func(t *testing.T) {
		sets := []Set{
			{Name: "missing", MainFile: "missing.par2"},
		}
		renames, err := assessAndApply(t, t.TempDir(), sets, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected 0 renames, got %d", len(renames))
		}
	})
}

func TestCollectManifests_Standalone(t *testing.T) {
	dir := t.TempDir()
	parPath := filepath.Join(dir, "test.par2")
	setID := [16]byte{1}
	fileID := [16]byte{2}
	body := buildFileDescBody(fileID, [16]byte{}, [16]byte{}, 100, "Sub/test.dat")
	pkt := buildPacket(setID, typeFileDesc, body)
	if err := os.WriteFile(parPath, pkt, 0o644); err != nil {
		t.Fatal(err)
	}

	sets := []Set{
		{Name: "empty"},
		{Name: "missing", MainFile: "nonexistent.par2"},
		{Name: "valid", MainFile: parPath},
	}
	got := collectManifests(sets, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 desc from valid set, got %d", len(got))
	}
	if got[0].FileName != "Sub/test.dat" {
		t.Errorf("unexpected desc: %+v", got[0])
	}
}

func TestScanFlatFiles_Standalone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := scanFlatFiles(dir, nil)
	if err != nil {
		t.Fatalf("scanFlatFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 flat file (ignoring subdir), got %d", len(got))
	}
	if _, ok := got["file1.txt"]; !ok {
		t.Errorf("expected file1.txt in map")
	}

	_, err = scanFlatFiles("/nonexistent_dir_for_test", nil)
	if err == nil {
		t.Error("expected error for nonexistent dir")
	}
}
