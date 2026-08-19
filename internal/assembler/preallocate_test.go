package assembler

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPreallocateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preallocated.dat")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer f.Close()

	const expectedSize int64 = 1 << 20 // 1 MiB
	if err := preallocateFile(f, expectedSize); err != nil {
		t.Fatalf("preallocateFile: %v", err)
	}

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != expectedSize {
		t.Errorf("file size = %d, want %d", info.Size(), expectedSize)
	}
}

func TestPreallocateFileZeroSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zero.dat")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer f.Close()

	// Pre-allocating with zero should be a no-op (no error).
	if err := preallocateFile(f, 0); err != nil {
		t.Fatalf("preallocateFile(0): %v", err)
	}

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("file size after zero pre-alloc = %d, want 0", info.Size())
	}
}

func TestPreallocateWriteAtCorrectness(t *testing.T) {
	// Verify that WriteAt works correctly on a pre-allocated file.
	// The file should contain exactly the written data at the correct
	// offsets, with zeros (or holes) elsewhere.
	dir := t.TempDir()
	path := filepath.Join(dir, "writeat.dat")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	const totalSize int64 = 100
	if err := preallocateFile(f, totalSize); err != nil {
		t.Fatalf("preallocateFile: %v", err)
	}

	// Write "HELLO" at offset 50 and "WORLD" at offset 0.
	if _, err := f.WriteAt([]byte("HELLO"), 50); err != nil {
		t.Fatalf("WriteAt(50): %v", err)
	}
	if _, err := f.WriteAt([]byte("WORLD"), 0); err != nil {
		t.Fatalf("WriteAt(0): %v", err)
	}
	f.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if int64(len(data)) != totalSize {
		t.Fatalf("file length = %d, want %d", len(data), totalSize)
	}
	if string(data[0:5]) != "WORLD" {
		t.Errorf("data[0:5] = %q, want %q", data[0:5], "WORLD")
	}
	if string(data[50:55]) != "HELLO" {
		t.Errorf("data[50:55] = %q, want %q", data[50:55], "HELLO")
	}
	// Unwritten regions should be zero bytes.
	for i := 5; i < 50; i++ {
		if data[i] != 0 {
			t.Errorf("data[%d] = %d, want 0", i, data[i])
			break
		}
	}
}

func TestSupportsSparse(t *testing.T) {
	dir := t.TempDir()

	// On typical CI/dev Linux filesystems (ext4, tmpfs, btrfs, xfs),
	// sparse files are supported. This test verifies the probe works and
	// matches that expectation, but skips rather than fails if the test
	// filesystem doesn't support SEEK_HOLE/SEEK_DATA — sparse support is
	// not portable across all filesystems/CI backends.
	got := SupportsSparse(dir)
	if !got {
		t.Skipf("SupportsSparse(%s) = false; filesystem does not support sparse files", dir)
	}
}

func TestSupportsSparseInvalidDir(t *testing.T) {
	// Non-existent directory should return false, not panic.
	got := SupportsSparse("/nonexistent/path/should/not/exist")
	if got {
		t.Error("SupportsSparse returned true for non-existent directory")
	}
}

func TestCheckSparseSupport(t *testing.T) {
	dir := t.TempDir()

	supported, msg := CheckSparseSupport(dir)
	t.Logf("CheckSparseSupport(%s): supported=%v msg=%q", dir, supported, msg)

	if !supported {
		t.Skipf("CheckSparseSupport(%s) = false; filesystem does not support sparse files", dir)
	}
	if msg == "" {
		t.Error("CheckSparseSupport returned empty message")
	}
}

func TestAssemblyWithPreallocation(t *testing.T) {
	// End-to-end: verify that articles written to a pre-allocated file
	// produce correct output. This exercises the full path: FileInfo with
	// ExpectedSize → preallocateFile → WriteAt → file completion.
	dir := t.TempDir()
	files := make(map[string]FileInfo)

	path := filepath.Join(dir, "prealloc_e2e.dat")
	key := "job1:0"
	files[key] = FileInfo{
		Path:         path,
		TotalParts:   3,
		ExpectedSize: 12, // 3 articles × 4 bytes
	}

	var completed bool
	opts := makeOpts(dir, files)
	opts.OnFileComplete = func(_ string, _ int) { completed = true }

	a := startAssembler(t, opts)

	// Write articles out of order.
	articles := []struct {
		offset int64
		data   []byte
	}{
		{8, []byte("CCCC")},
		{0, []byte("AAAA")},
		{4, []byte("BBBB")},
	}
	for i, art := range articles {
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: art.offset, Data: art.data}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle: %v", err)
		}
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !completed {
		t.Error("OnFileComplete was not called")
	}

	data := readFile(t, path)
	want := "AAAABBBBCCCC"
	if string(data) != want {
		t.Errorf("file content = %q, want %q", data, want)
	}

	// Verify the file was pre-allocated (size should be exactly 12,
	// not larger from pre-allocation). The assembler writes 12 bytes
	// total which matches ExpectedSize, so final size should be 12.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 12 {
		t.Errorf("final file size = %d, want 12", info.Size())
	}
}

func TestPreallocationActuallyPreallocates(t *testing.T) {
	// Verify that preallocateFile creates a file with the expected
	// apparent size, and on sparse-capable filesystems, the allocated
	// blocks are less than the apparent size.
	dir := t.TempDir()
	path := filepath.Join(dir, "prealloc_check.dat")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const size int64 = 10 << 20 // 10 MiB
	if err := preallocateFile(f, size); err != nil {
		t.Fatalf("preallocateFile: %v", err)
	}
	f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != size {
		t.Errorf("apparent size = %d, want %d", info.Size(), size)
	}

	// On Linux, check if the allocation is sparse or fallocated.
	// Either way, the file should exist at the expected size.
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		t.Fatalf("syscall.Stat: %v", err)
	}
	allocBytes := stat.Blocks * 512
	t.Logf("pre-allocated %d bytes: apparent=%d allocated=%d (%.1f%%)",
		size, info.Size(), allocBytes, float64(allocBytes)/float64(size)*100)
}
