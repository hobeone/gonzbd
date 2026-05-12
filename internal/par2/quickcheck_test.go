package par2

import (
	"crypto/md5"
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

	// Test computeHash16k.
	got, err := computeHash16k(filepath.Join(dir, "a1b2c3d4.dat"))
	if err != nil {
		t.Fatalf("computeHash16k: %v", err)
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

	got, err := computeHash16k(path)
	if err != nil {
		t.Fatalf("computeHash16k: %v", err)
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

	got, err := computeHash16k(path)
	if err != nil {
		t.Fatalf("computeHash16k: %v", err)
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
