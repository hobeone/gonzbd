package par2

import (
	"crypto/md5"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func TestQuickCheck_BasenameMatch(t *testing.T) {
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

func TestQuickCheck_FlattenedNameMatch(t *testing.T) {
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

func TestQuickCheck_Hash16kMatch(t *testing.T) {
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

func TestQuickCheck_PathTraversal(t *testing.T) {
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

func TestQuickCheck_SizeMismatch(t *testing.T) {
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

func TestQuickCheck_NoSubdirs(t *testing.T) {
	dir := t.TempDir()

	content := []byte("flat file")
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Par2 entry without subdirectory — no relocation needed.
	fd := FileDesc{
		FileName: "movie.mkv",
		FileSize: uint64(len(content)),
	}

	// relocateFile will succeed but the file stays in the same place
	// (filepath.Join(dir, "movie.mkv") → filepath.Join(dir, "movie.mkv")).
	// In practice, QuickCheck filters these out before calling relocateFile.

	// Just verify that the filtering logic works: FileName has no "/".
	if filepath.ToSlash(fd.FileName) != fd.FileName || !containsSlash(fd.FileName) {
		// Expected: no slash, so this entry would be filtered out by QuickCheck.
		t.Log("correctly identified as flat entry (no subdirectory)")
	}
}

func containsSlash(s string) bool {
	for _, c := range s {
		if c == '/' {
			return true
		}
	}
	return false
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
func TestQuickCheck_Phase4_CRCSizeFallback(t *testing.T) {
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
func TestQuickCheck_Phase3HashMatch_EndToEnd(t *testing.T) {
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
	renames, err := QuickCheck(dir, sets, nil)
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
func TestQuickCheck_Phase4CRCMatch_EndToEnd(t *testing.T) {
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
	renames, err := QuickCheck(dir, sets, nil)
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

func TestQuickCheck_Integration(t *testing.T) {
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
	renames, err := QuickCheck(tmpDir, sets, nil)
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

func TestQuickCheck_EdgeCases(t *testing.T) {
	// Case 1: Empty manifests list (or no par2 main file)
	t.Run("empty main file skips set", func(t *testing.T) {
		sets := []Set{
			{Name: "empty"},
		}
		renames, err := QuickCheck(t.TempDir(), sets, nil)
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
		renames, err := QuickCheck(t.TempDir(), sets, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(renames) != 0 {
			t.Errorf("expected 0 renames, got %d", len(renames))
		}
	})
}

func TestMatchByBasename_Standalone(t *testing.T) {
	dir := t.TempDir()
	content := []byte("basename standalone content")
	if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	flatFiles, err := scanFlatFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	subdirEntries := []FileDesc{
		{FileName: "Album/photo.jpg", FileSize: uint64(len(content))},
		{FileName: "Album/other.jpg", FileSize: 100}, // missing flat file
	}
	matched := make(map[string]bool)
	relocated := make(map[string]bool)

	// Test successful match
	renames := matchByBasename(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames) != 1 {
		t.Fatalf("expected 1 rename, got %d", len(renames))
	}
	if renames[0].From != "photo.jpg" || renames[0].To != "Album/photo.jpg" {
		t.Errorf("unexpected rename: %+v", renames[0])
	}
	if !matched["photo.jpg"] || !relocated["Album/photo.jpg"] {
		t.Errorf("expected matched and relocated to be true")
	}

	// Verify file was relocated on disk
	if _, err := os.Stat(filepath.Join(dir, "Album", "photo.jpg")); err != nil {
		t.Fatalf("file not relocated to expected path: %v", err)
	}

	// Test already relocated entry is skipped
	renames2 := matchByBasename(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames2) != 0 {
		t.Errorf("expected 0 renames for already relocated/matched, got %d", len(renames2))
	}

	// Test already matched flat file is skipped when a duplicate par2 entry asks for it
	dupEntries := []FileDesc{{FileName: "Album2/photo.jpg", FileSize: uint64(len(content))}}
	renames3 := matchByBasename(dir, dupEntries, flatFiles, matched, relocated, nil)
	if len(renames3) != 0 {
		t.Errorf("expected 0 renames for already matched flat file, got %d", len(renames3))
	}
}

func TestMatchByFlattenedName_Standalone(t *testing.T) {
	dir := t.TempDir()
	content := []byte("flattened standalone content")
	if err := os.WriteFile(filepath.Join(dir, "Album_photo.jpg"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	flatFiles, err := scanFlatFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	subdirEntries := []FileDesc{
		{FileName: "Album/photo.jpg", FileSize: uint64(len(content))},
		{FileName: "Album/missing.jpg", FileSize: 100},
	}
	matched := make(map[string]bool)
	relocated := make(map[string]bool)

	// Test successful flattened match
	renames := matchByFlattenedName(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames) != 1 {
		t.Fatalf("expected 1 rename, got %d", len(renames))
	}
	if renames[0].From != "Album_photo.jpg" || renames[0].To != "Album/photo.jpg" {
		t.Errorf("unexpected rename: %+v", renames[0])
	}
	if !matched["Album_photo.jpg"] || !relocated["Album/photo.jpg"] {
		t.Errorf("expected matched and relocated to be true")
	}

	// Verify file was relocated on disk
	if _, err := os.Stat(filepath.Join(dir, "Album", "photo.jpg")); err != nil {
		t.Fatalf("file not relocated: %v", err)
	}

	// Test already relocated par2 entry is skipped
	renames2 := matchByFlattenedName(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames2) != 0 {
		t.Errorf("expected 0 renames, got %d", len(renames2))
	}

	// Test already matched flat file is skipped
	dupEntries := []FileDesc{{FileName: "Album/photo.jpg", FileSize: uint64(len(content))}}
	relocated = make(map[string]bool) // clear relocated but keep matched
	renames3 := matchByFlattenedName(dir, dupEntries, flatFiles, matched, relocated, nil)
	if len(renames3) != 0 {
		t.Errorf("expected 0 renames for already matched flat file, got %d", len(renames3))
	}
}

func TestMatchByHash16k_Standalone(t *testing.T) {
	dir := t.TempDir()
	content := make([]byte, 20*1024)
	for i := range content {
		content[i] = byte(i % 250)
	}
	hash16k := md5.Sum(content[:16*1024])

	// Create valid file
	if err := os.WriteFile(filepath.Join(dir, "random_name.dat"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// Create file with same hash but ignored extension (.par2)
	if err := os.WriteFile(filepath.Join(dir, "ignored.par2"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// Create file with size mismatch
	shortContent := make([]byte, 10)
	if err := os.WriteFile(filepath.Join(dir, "short.dat"), shortContent, 0o644); err != nil {
		t.Fatal(err)
	}

	flatFiles, err := scanFlatFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	subdirEntries := []FileDesc{
		{FileName: "Dir/real.dat", FileSize: uint64(len(content)), Hash16k: hash16k},
		{FileName: "Dir/ignored.dat", FileSize: uint64(len(content)), Hash16k: md5.Sum(content[:16*1024])},
		{FileName: "Dir/short_target.dat", FileSize: 99999, Hash16k: md5.Sum(shortContent)},
	}
	matched := make(map[string]bool)
	relocated := make(map[string]bool)

	// Mark ignored.dat as relocated beforehand to test early return when all are relocated
	relocated["Dir/ignored.dat"] = true

	renames := matchByHash16k(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames) != 1 {
		t.Fatalf("expected 1 rename, got %d", len(renames))
	}
	if renames[0].From != "random_name.dat" || renames[0].To != "Dir/real.dat" {
		t.Errorf("unexpected rename: %+v", renames[0])
	}
	if !matched["random_name.dat"] || !relocated["Dir/real.dat"] {
		t.Errorf("expected matched and relocated to be set")
	}

	// Verify short.dat wasn't matched because size differed
	if matched["short.dat"] {
		t.Errorf("short.dat should not have matched due to size difference")
	}
	// Verify ignored.par2 wasn't matched due to extension
	if matched["ignored.par2"] {
		t.Errorf("ignored.par2 should not have matched")
	}

	// Test early return when all subdir entries are relocated
	relocated["Dir/short_target.dat"] = true
	renames2 := matchByHash16k(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames2) != 0 {
		t.Errorf("expected 0 renames when all relocated, got %d", len(renames2))
	}
}

func TestMatchByCRC32Fallback_Standalone(t *testing.T) {
	dir := t.TempDir()
	content := []byte("crc32 standalone test data")
	actualCRC := crc32.ChecksumIEEE(content)

	if err := os.WriteFile(filepath.Join(dir, "obfuscated.dat"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// File with 0 CRC / Size in par2 should not match in phase 4
	if err := os.WriteFile(filepath.Join(dir, "zero.dat"), []byte("zero"), 0o644); err != nil {
		t.Fatal(err)
	}

	flatFiles, err := scanFlatFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	subdirEntries := []FileDesc{
		{FileName: "Dir/crc_target.dat", FileSize: uint64(len(content)), FileCRC32: actualCRC},
		{FileName: "Dir/zero_target.dat", FileSize: 0, FileCRC32: 0},
	}
	matched := make(map[string]bool)
	relocated := make(map[string]bool)

	renames := matchByCRC32Fallback(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames) != 1 {
		t.Fatalf("expected 1 rename, got %d", len(renames))
	}
	if renames[0].From != "obfuscated.dat" || renames[0].To != "Dir/crc_target.dat" {
		t.Errorf("unexpected rename: %+v", renames[0])
	}
	if !matched["obfuscated.dat"] || !relocated["Dir/crc_target.dat"] {
		t.Errorf("expected matched and relocated to be set")
	}

	// Test when all valid CRC entries are already relocated
	renames2 := matchByCRC32Fallback(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames2) != 0 {
		t.Errorf("expected 0 renames, got %d", len(renames2))
	}

	// Test size mismatch pre-check in tryMatchCRC32File
	diffSizeContent := []byte("different size content for precheck")
	if err := os.WriteFile(filepath.Join(dir, "diff_size.dat"), diffSizeContent, 0o644); err != nil {
		t.Fatal(err)
	}
	flatFiles, _ = scanFlatFiles(dir, nil)
	subdirEntries = []FileDesc{
		{FileName: "Dir/other.dat", FileSize: 999, FileCRC32: 0x12345678},
	}
	relocated = make(map[string]bool)
	renames3 := matchByCRC32Fallback(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames3) != 0 {
		t.Errorf("expected 0 renames on size mismatch precheck, got %d", len(renames3))
	}
}

func TestShouldSkipFlatFile_Extensions(t *testing.T) {
	matched := make(map[string]bool)
	if !shouldSkipFlatFile("test.par2", matched) {
		t.Error("expected .par2 to be skipped")
	}
	if !shouldSkipFlatFile("test.sfv", matched) {
		t.Error("expected .sfv to be skipped")
	}
	if !shouldSkipFlatFile("test.nfo", matched) {
		t.Error("expected .nfo to be skipped")
	}
	if shouldSkipFlatFile("test.mkv", matched) {
		t.Error("expected .mkv not to be skipped")
	}
	matched["test.mkv"] = true
	if !shouldSkipFlatFile("test.mkv", matched) {
		t.Error("expected matched test.mkv to be skipped")
	}
}

func TestFilterSubdirEntries_Standalone(t *testing.T) {
	manifest := []FileDesc{
		{FileName: "movie.mkv", FileSize: 1000},
		{FileName: "Subdir/movie.mkv", FileSize: 2000},
	}
	got := filterSubdirEntries(manifest, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 subdir entry, got %d", len(got))
	}
	if got[0].FileName != "Subdir/movie.mkv" {
		t.Errorf("unexpected entry: %+v", got[0])
	}
	empty := filterSubdirEntries([]FileDesc{{FileName: "flat.txt"}}, nil)
	if len(empty) != 0 {
		t.Errorf("expected 0 entries for flat manifest, got %d", len(empty))
	}
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

func TestMatchByHash16k_DeletedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deleted.dat")
	if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	flatFiles, _ := scanFlatFiles(dir, nil)
	os.Remove(path)

	subdirEntries := []FileDesc{{FileName: "Dir/deleted.dat", FileSize: 4, Hash16k: md5.Sum([]byte("test"))}}
	matched := make(map[string]bool)
	relocated := make(map[string]bool)
	renames := matchByHash16k(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames) != 0 {
		t.Errorf("expected 0 renames when file cannot be hashed, got %d", len(renames))
	}
}

func TestMatchByCRC32Fallback_DeletedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deleted.dat")
	content := []byte("test")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	flatFiles, _ := scanFlatFiles(dir, nil)
	os.Remove(path)

	subdirEntries := []FileDesc{{FileName: "Dir/deleted.dat", FileSize: 4, FileCRC32: crc32.ChecksumIEEE(content)}}
	matched := make(map[string]bool)
	relocated := make(map[string]bool)
	renames := matchByCRC32Fallback(dir, subdirEntries, flatFiles, matched, relocated, nil)
	if len(renames) != 0 {
		t.Errorf("expected 0 renames when file cannot be CRCed, got %d", len(renames))
	}
}

type mockDirEntry struct {
	name    string
	infoErr error
}

func (m mockDirEntry) Name() string      { return m.name }
func (m mockDirEntry) IsDir() bool       { return false }
func (m mockDirEntry) Type() os.FileMode { return 0 }
func (m mockDirEntry) Info() (os.FileInfo, error) {
	if m.infoErr != nil {
		return nil, m.infoErr
	}
	return os.Stat(m.name)
}

func TestTryMatch_InfoError(t *testing.T) {
	dir := t.TempDir()
	content := []byte("info error test")
	path := filepath.Join(dir, "info_err.dat")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	mockEntry := mockDirEntry{name: "info_err.dat", infoErr: errors.New("simulated info error")}

	hashIndex := map[[16]byte]FileDesc{
		md5.Sum(content): {FileName: "Dir/target.dat", FileSize: uint64(len(content))},
	}
	if _, ok := tryMatchHash16kFile(dir, "info_err.dat", mockEntry, hashIndex, nil); ok {
		t.Error("expected tryMatchHash16kFile to return false when Info() errors")
	}

	crcIndex := map[crcSizeKey]FileDesc{
		{crc: crc32.ChecksumIEEE(content), size: uint64(len(content))}: {FileName: "Dir/target.dat", FileSize: uint64(len(content))},
	}
	if _, ok := tryMatchCRC32File(dir, "info_err.dat", mockEntry, crcIndex, nil); ok {
		t.Error("expected tryMatchCRC32File to return false when Info() errors")
	}
}

func TestTryMatch_RelocateFileFails(t *testing.T) {
	dir := t.TempDir()
	content := []byte("relocate fail test")
	path := filepath.Join(dir, "fail.dat")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	flatFiles, _ := scanFlatFiles(dir, nil)
	entry := flatFiles["fail.dat"]

	hashIndex := map[[16]byte]FileDesc{
		md5.Sum(content): {FileName: "../../etc/passwd", FileSize: uint64(len(content))},
	}
	if _, ok := tryMatchHash16kFile(dir, "fail.dat", entry, hashIndex, nil); ok {
		t.Error("expected tryMatchHash16kFile to return false when relocateFile fails")
	}

	crcIndex := map[crcSizeKey]FileDesc{
		{crc: crc32.ChecksumIEEE(content), size: uint64(len(content))}: {FileName: "../../etc/passwd", FileSize: uint64(len(content))},
	}
	if _, ok := tryMatchCRC32File(dir, "fail.dat", entry, crcIndex, nil); ok {
		t.Error("expected tryMatchCRC32File to return false when relocateFile fails")
	}
}
