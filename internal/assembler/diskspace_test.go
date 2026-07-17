package assembler

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestFreeBytes_RejectsUncancellableContext(t *testing.T) {
	_, err := FreeBytes(context.Background(), t.TempDir()) //nolint:usetesting // deliberately testing context.Background(), not t.Context()
	if err == nil {
		t.Fatal("expected error for context.Background() (no Done() channel), got nil")
	}
}

// TestCheckDiskSpace_SurvivesHungStatfs proves that checkDiskSpace bounds its
// FreeBytes call: it must return within diskCheckTimeout even if the
// underlying statfs syscall never returns (simulating a stuck NFS/SMB mount).
//
// It swaps the package-level statfs var for a hook that blocks until the test
// unblocks it — simulating exactly the "stuck mount" scenario FreeBytes' ctx
// parameter exists to bound. On unpatched code (checkDiskSpace passing
// context.Background()), this test must time out waiting for checkDiskSpace
// to return, since nothing would ever unblock the ctx.Done() arm of
// FreeBytes' select.
func TestCheckDiskSpace_SurvivesHungStatfs(t *testing.T) {
	origStatfs := statfs
	unblock := make(chan struct{})
	statfsReturned := make(chan struct{})
	statfs = func(path string, buf *syscall.Statfs_t) error {
		<-unblock
		err := origStatfs(path, buf)
		close(statfsReturned)
		return err
	}
	t.Cleanup(func() {
		close(unblock)
		// Wait for the abandoned goroutine's call to actually finish before
		// restoring statfs: it reads the package var once, at the call site
		// below, and that read happens-before this close (same goroutine,
		// program order) — so waiting on it here avoids racing the
		// reassignment against that read.
		<-statfsReturned
		statfs = origStatfs
	})

	dir := t.TempDir()
	var lowDiskCalls int
	opts := Options{
		FileInfo: func(string, int) (FileInfo, error) {
			return FileInfo{}, nil
		},
		OnLowDisk: func(string, int64) { lowDiskCalls++ },
	}
	a := New(opts, nil)
	a.SetMinFreeBytes(1) // any positive value enables the check

	open := map[fileKey]*openFile{
		{jobID: "job1", fileIdx: 0}: {info: FileInfo{Path: dir + "/file.bin"}},
	}

	done := make(chan struct{})
	go func() {
		a.checkDiskSpace(open)
		close(done)
	}()

	select {
	case <-done:
		// checkDiskSpace returned promptly despite the hung statfs — proves
		// it now bounds the call instead of blocking forever.
	case <-time.After(diskCheckTimeout + 5*time.Second):
		t.Fatal("checkDiskSpace did not return within the bounded timeout; " +
			"a hung statfs would stall the assembler worker indefinitely")
	}
}

func TestWriteSpeedMBPerSec_WritesAndCleansUp(t *testing.T) {
	dir := t.TempDir()

	mbPerSec, err := WriteSpeedMBPerSec(context.Background(), dir, 4*1024*1024) // 4 MiB for a fast test
	if err != nil {
		t.Fatalf("WriteSpeedMBPerSec: %v", err)
	}
	// Sane order-of-magnitude bounds on the computed throughput, not just
	// "positive": a 4 MiB write completing in well under a second is
	// physically bounded between roughly 0.1 MB/s (pathologically slow) and
	// 100,000 MB/s (100 GB/s, far beyond any real disk) — wide enough to
	// never flake on real hardware, but tight enough that swapping the
	// mb/elapsed division for a multiplication (or vice versa) produces a
	// value far outside this range.
	const minPlausibleMBPerSec = 0.1
	const maxPlausibleMBPerSec = 100_000
	if mbPerSec < minPlausibleMBPerSec || mbPerSec > maxPlausibleMBPerSec {
		t.Errorf("mbPerSec = %f, want in [%v, %v]", mbPerSec, minPlausibleMBPerSec, maxPlausibleMBPerSec)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected temp file to be cleaned up, found %d entries in %s: %v", len(entries), dir, entries)
	}
}

func TestWriteSpeedMBPerSec_NonexistentDir(t *testing.T) {
	_, err := WriteSpeedMBPerSec(context.Background(), "/nonexistent/path/that/does/not/exist", 1024)
	if err == nil {
		t.Fatal("expected error for nonexistent directory, got nil")
	}
}

func TestWriteSpeedMBPerSec_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := WriteSpeedMBPerSec(ctx, dir, 4*1024*1024)
	if err == nil {
		t.Fatal("expected error for already-cancelled context, got nil")
	}
}
