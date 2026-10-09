package storagefault

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func shrinkFile(t *testing.T, size int64) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "f.bin"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	return f
}

func sizeOf(t *testing.T, f *os.File) int64 {
	t.Helper()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func TestShrinkAndSync_Sizes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		size, end int64
		want      int64
		wantSyncs int
	}{
		{"larger is truncated", 500, 300, 300, 2},
		{"shorter is not grown", 200, 300, 200, 2},
		{"no bound leaves it alone", 500, 0, 500, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := shrinkFile(t, tc.size)
			syncs := 0
			if err := ShrinkAndSync(f, tc.end, func(*os.File) error { syncs++; return nil }); err != nil {
				t.Fatal(err)
			}
			if got := sizeOf(t, f); got != tc.want {
				t.Errorf("size = %d, want %d", got, tc.want)
			}
			if syncs != tc.wantSyncs {
				t.Errorf("fsynced %d times, want %d", syncs, tc.wantSyncs)
			}
		})
	}
}

func TestShrinkAndSync_FirstFsyncFailureDoesNotTruncate(t *testing.T) {
	t.Parallel()
	f := shrinkFile(t, 500)
	err := ShrinkAndSync(f, 300, func(*os.File) error { return syscall.EIO })
	var fault *Fault
	if !errors.As(err, &fault) || fault.Op != "sync" || !errors.Is(err, syscall.EIO) {
		t.Fatalf("err = %v, want a sync Fault wrapping EIO", err)
	}
	if got := sizeOf(t, f); got != 500 {
		t.Errorf("size = %d after a failed first fsync, want 500", got)
	}
}

func TestShrinkAndSync_SecondFsyncFailureIsReturned(t *testing.T) {
	t.Parallel()
	f := shrinkFile(t, 500)
	calls := 0
	err := ShrinkAndSync(f, 300, func(*os.File) error {
		calls++
		if calls == 2 {
			return syscall.EIO
		}
		return nil
	})
	var fault *Fault
	if !errors.As(err, &fault) || fault.Op != "sync" || !errors.Is(err, syscall.EIO) {
		t.Fatalf("err = %v, want a sync Fault wrapping EIO from the second fsync", err)
	}
}

func TestShrinkAndSync_TruncateAndStatFailuresAreClassified(t *testing.T) {
	t.Parallel()
	f := shrinkFile(t, 500)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err := ShrinkAndSync(f, 300, func(*os.File) error { return nil })
	var fault *Fault
	if !errors.As(err, &fault) || fault.Op != "stat" {
		t.Fatalf("err = %v, want a stat Fault on a closed file", err)
	}
}
