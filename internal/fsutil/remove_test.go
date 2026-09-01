package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/testutil"
)

func TestIsSillyRenameFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{".nfs00000000000cc20f00000001", true},
		{".nfsA", true},
		{".smbdelete0123456789abcd", true},
		{".fuse_hidden00000001", true},
		{"normal.txt", false},
		{".hidden", false},
		{"nfs_share.txt", false},
	}
	for _, tc := range tests {
		if got := IsSillyRenameFile(tc.name); got != tc.want {
			t.Errorf("IsSillyRenameFile(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestContainsOnlySillyRenames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.AssertNoFDLeaks(t, dir)

	// Empty dir is not considered containing silly renames (nothing to delay for).
	if ok, _, err := ContainsOnlySillyRenames(dir); err != nil || ok {
		t.Errorf("empty dir: got (%v, %v), want (false, nil)", ok, err)
	}

	// Add an NFS silly rename file in a subdirectory.
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	nfsFile := filepath.Join(sub, ".nfs00000000000cc20f00000001")
	if err := os.WriteFile(nfsFile, []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}

	ok, files, err := ContainsOnlySillyRenames(dir)
	if err != nil || !ok || len(files) != 1 || files[0] != nfsFile {
		t.Errorf("only silly renames: got (%v, %v, %v), want (true, [%s], nil)", ok, files, err, nfsFile)
	}

	// Add a normal file. Now it should return false.
	normalFile := filepath.Join(dir, "video.mkv")
	if err := os.WriteFile(normalFile, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, _, err = ContainsOnlySillyRenames(dir)
	if err != nil || ok {
		t.Errorf("with normal file: got (%v, %v), want (false, nil)", ok, err)
	}
}

func TestIsBusyOrNotEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"EBUSY", syscall.EBUSY, true},
		{"ENOTEMPTY", syscall.ENOTEMPTY, true},
		{"EEXIST", syscall.EEXIST, true},
		{"EPERM", syscall.EPERM, true},
		{"EACCES", syscall.EACCES, true},
		{"ENOENT", syscall.ENOENT, false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		if got := isBusyOrNotEmpty(tc.err); got != tc.want {
			t.Errorf("isBusyOrNotEmpty(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Must stay non-parallel: mutates the shared removeBackoffs package var.
// t.Parallel siblings pause until this test returns, so the mutate/restore
// pair here is safe only as long as this test runs serially.
func TestSetRemoveBackoffsForTest(t *testing.T) {
	origBackoffs := removeBackoffs
	t.Cleanup(func() { removeBackoffs = origBackoffs })

	shortened := []time.Duration{time.Millisecond}
	restore := SetRemoveBackoffsForTest(shortened)
	if len(removeBackoffs) != 1 || removeBackoffs[0] != time.Millisecond {
		t.Fatalf("removeBackoffs after override = %v, want %v", removeBackoffs, shortened)
	}

	restore()
	if len(removeBackoffs) != len(origBackoffs) {
		t.Fatalf("removeBackoffs after restore = %v, want original schedule %v", removeBackoffs, origBackoffs)
	}
}

func TestRemoveAll_RetryAndSillyRenameDetection(t *testing.T) {
	dir := t.TempDir()
	testutil.AssertNoFDLeaks(t, dir)
	nfsFile := filepath.Join(dir, ".nfs00000000000cc20f00000001")
	if err := os.WriteFile(nfsFile, []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}

	origRemoveAll := removeAllFunc
	origBackoffs := removeBackoffs
	defer func() {
		removeAllFunc = origRemoveAll
		removeBackoffs = origBackoffs
	}()
	removeBackoffs = []time.Duration{time.Millisecond, time.Millisecond}

	// Test 1: Retry succeeds on second attempt.
	attempts := 0
	removeAllFunc = func(path string) error {
		attempts++
		if attempts < 2 {
			return syscall.EBUSY
		}
		return origRemoveAll(path)
	}
	if err := RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll retry failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}

	// Recreate directory and file for Test 2.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nfsFile, []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Test 2: Persistent EBUSY with only silly renames logs warning and returns nil (non-fatal).
	removeAllFunc = func(path string) error {
		return syscall.EBUSY
	}
	if err := RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll persistent EBUSY with silly renames should be ignored, got: %v", err)
	}

	// Test 3: Persistent EBUSY with a normal file returns the error.
	normalFile := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(normalFile, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(dir); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("RemoveAll persistent EBUSY with real files: got %v, want EBUSY", err)
	}
}

func TestRemove_RetryAndSillyRenameDetection(t *testing.T) {
	dir := t.TempDir()
	testutil.AssertNoFDLeaks(t, dir)
	nfsFile := filepath.Join(dir, ".nfs00000000000cc20f00000001")
	if err := os.WriteFile(nfsFile, []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}

	origRemove := removeFunc
	origBackoffs := removeBackoffs
	defer func() {
		removeFunc = origRemove
		removeBackoffs = origBackoffs
	}()
	removeBackoffs = []time.Duration{time.Millisecond, time.Millisecond}

	// Test 1: Retry succeeds on second attempt.
	attempts := 0
	removeFunc = func(path string) error {
		attempts++
		if attempts < 2 {
			return syscall.EBUSY
		}
		return origRemove(path)
	}
	if err := Remove(nfsFile); err != nil {
		t.Fatalf("Remove retry failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}

	// Recreate file for Test 2.
	if err := os.WriteFile(nfsFile, []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Test 2: Persistent EBUSY on a silly rename file returns nil (non-fatal).
	removeFunc = func(path string) error {
		return syscall.EBUSY
	}
	if err := Remove(nfsFile); err != nil {
		t.Fatalf("Remove persistent EBUSY on silly rename should be ignored, got: %v", err)
	}

	// Test 3: Persistent EBUSY on a normal file returns error.
	normalFile := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(normalFile, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Remove(normalFile); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("Remove persistent EBUSY on real file: got %v, want EBUSY", err)
	}
}

func TestRemoveRootAll_RetryAndSillyRenameDetection(t *testing.T) {
	dir := t.TempDir()
	testutil.AssertNoFDLeaks(t, dir)
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	nfsFile := filepath.Join(sub, ".nfs00000000000cc20f00000001")
	if err := os.WriteFile(nfsFile, []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	origRootRemoveAll := rootRemoveAllFunc
	origBackoffs := removeBackoffs
	defer func() {
		rootRemoveAllFunc = origRootRemoveAll
		removeBackoffs = origBackoffs
	}()
	removeBackoffs = []time.Duration{time.Millisecond, time.Millisecond}

	// Test 1: Retry succeeds on second attempt.
	attempts := 0
	rootRemoveAllFunc = func(r *os.Root, name string) error {
		attempts++
		if attempts < 2 {
			return syscall.EBUSY
		}
		return origRootRemoveAll(r, name)
	}
	if err := RemoveRootAll(root, "sub", filepath.Join(dir, "sub")); err != nil {
		t.Fatalf("RemoveRootAll retry failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}

	// Recreate directory and file for Test 2.
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nfsFile, []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Test 2: Persistent EBUSY with only silly renames logs warning and returns nil.
	rootRemoveAllFunc = func(r *os.Root, name string) error {
		return syscall.EBUSY
	}
	if err := RemoveRootAll(root, "sub", filepath.Join(dir, "sub")); err != nil {
		t.Fatalf("RemoveRootAll persistent EBUSY with silly renames should be ignored, got: %v", err)
	}

	// Test 3: Persistent EBUSY with real files returns error.
	normalFile := filepath.Join(sub, "real.txt")
	if err := os.WriteFile(normalFile, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveRootAll(root, "sub", filepath.Join(dir, "sub")); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("RemoveRootAll persistent EBUSY with real files: got %v, want EBUSY", err)
	}
}
