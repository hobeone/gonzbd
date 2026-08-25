package assembler

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// assertNoLeakedGoroutines fails t if any goroutine started after baseline
// (a goleak.IgnoreCurrent() snapshot taken before the code under test ran)
// is still alive. Shared by tests that deliberately park a fake statfs on a
// channel to simulate a hung/abandoned probe, then unblock it and must
// prove the goroutine actually terminates rather than merely that callers
// stopped waiting on it.
func assertNoLeakedGoroutines(t *testing.T, baseline goleak.Option) {
	t.Helper()
	if err := goleak.Find(baseline); err != nil {
		t.Fatalf("goroutine did not terminate after being unblocked: %v", err)
	}
}

func TestFreeBytes_RejectsInvalidContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context //nolint:containedctx // table-driven test input, not stored state
	}{
		{"nil", nil},
		{"background", context.Background()}, //nolint:usetesting // deliberately testing context.Background(), not t.Context()
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FreeBytes(tt.ctx, t.TempDir())
			if err == nil {
				t.Fatalf("expected error for %s context, got nil", tt.name)
			}
		})
	}
}

// TestCheckDiskSpace_SurvivesHungStatfs proves that checkDiskSpace bounds its
// FreeBytes call: it must return within diskCheckTimeout even when the
// underlying call behaves like the real FreeBytes does on a stuck NFS/SMB
// mount — i.e. it only ever returns once its ctx is done, and never on its
// own. On unpatched code (checkDiskSpace passing context.Background(), whose
// Done() is nil), <-ctx.Done() below blocks forever, so this test must time
// out waiting for checkDiskSpace to return.
//
// a.diskProbe is a per-instance field (not shared/global state), set once
// before checkDiskSpace ever runs on this Assembler and never reassigned
// concurrently — the goroutine that calls checkDiskSpace is spawned after
// the field is set, so Go's happens-before guarantee for goroutine creation
// covers it without needing any additional synchronization. The fake statfs
// is injected below diskProbe (it has no ctx parameter — DiskProbe's probe
// goroutine is detached from any single caller's ctx by design), so
// simulating "hung" means blocking on a channel this test controls, not
// ctx.Done().
func TestCheckDiskSpace_SurvivesHungStatfs(t *testing.T) {
	baseline := goleak.IgnoreCurrent()

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
	unblock := make(chan struct{})
	a.diskProbe.statfs = func(path string, buf *syscall.Statfs_t) error {
		<-unblock
		return syscall.Statfs(path, buf)
	}

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

	// Release the abandoned probe goroutine and prove it actually
	// terminates — this test must not itself leak a permanently-parked
	// goroutine for the rest of the test binary's life.
	close(unblock)
	assertNoLeakedGoroutines(t, baseline)
}

// TestFreeBytes_AbandonedGoroutineCompletesAfterCancellation proves the part
// of FreeBytes' contract that TestCheckDiskSpace_SurvivesHungStatfs cannot:
// when ctx fires before the underlying statfs call returns, the abandoned
// goroutine is not left permanently blocked — it completes once statfs
// eventually does, and its buffered result channel absorbs the late send
// without deadlocking. This exercises freeBytes' real goroutine+select
// implementation directly (via the unexported helper and an injected fake),
// unlike the Assembler.freeBytes-level test above, which replaces that
// implementation entirely and so cannot observe this behavior.
//
// Waiting on a channel closed from inside the fake statfs function only
// proves the fake itself returned — not that freeBytes' enclosing goroutine
// went on to perform its ch <- result send (the actual leak-prone step, if
// ch were ever changed from buffered to unbuffered). goleak.Find, given a
// baseline recorded before freeBytes is even called, polls until every
// goroutine started after that baseline — including the one spawned inside
// freeBytes — has actually terminated, covering the whole goroutine body.
func TestFreeBytes_AbandonedGoroutineCompletesAfterCancellation(t *testing.T) {
	baseline := goleak.IgnoreCurrent()

	unblockStatfs := make(chan struct{})
	fakeStatfs := func(path string, buf *syscall.Statfs_t) error {
		<-unblockStatfs
		return syscall.Statfs(path, buf)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := freeBytes(ctx, t.TempDir(), fakeStatfs)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("freeBytes error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("freeBytes took %v to return after ctx expired, want prompt return", elapsed)
	}

	// Release the abandoned goroutine's statfs call and prove the whole
	// goroutine — statfs call plus the subsequent buffered channel send —
	// actually terminates, rather than merely that our fake function returned.
	close(unblockStatfs)
	if err := goleak.Find(baseline); err != nil {
		t.Fatalf("abandoned goroutine did not terminate after being unblocked: %v", err)
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

// TestStatfsFreeBytes covers the arithmetic both callers share.
//
// freeBytes and DiskProbe reach it by different routes — one ctx-bounded, one
// a detached single-flight probe — so a mistake here is wrong in two places at
// once, and neither caller's own tests look at the multiplication.
func TestStatfsFreeBytes(t *testing.T) {
	t.Run("multiplies available blocks by block size", func(t *testing.T) {
		free, err := statfsFreeBytes("/fake", func(_ string, buf *syscall.Statfs_t) error {
			buf.Bavail = 1000
			buf.Bsize = 4096
			return nil
		})
		if err != nil {
			t.Fatalf("statfsFreeBytes: %v", err)
		}
		if want := int64(1000 * 4096); free != want {
			t.Errorf("free = %d, want %d — Bavail is BLOCKS, not bytes, so a caller "+
				"comparing this against MinFreeBytes would be off by the block size",
				free, want)
		}
	})

	t.Run("reports zero free space as zero rather than an error", func(t *testing.T) {
		free, err := statfsFreeBytes("/fake", func(_ string, buf *syscall.Statfs_t) error {
			buf.Bavail = 0
			buf.Bsize = 4096
			return nil
		})
		if err != nil {
			t.Fatalf("statfsFreeBytes: %v", err)
		}
		if free != 0 {
			t.Errorf("free = %d, want 0", free)
		}
	})

	t.Run("wraps the statfs error and names the directory", func(t *testing.T) {
		_, err := statfsFreeBytes("/mnt/gone", func(string, *syscall.Statfs_t) error {
			return syscall.EACCES
		})
		if !errors.Is(err, syscall.EACCES) {
			t.Fatalf("err = %v, want it to wrap EACCES — the caller classifies storage "+
				"faults off this error, and an unwrapped one is unclassifiable", err)
		}
		if !strings.Contains(err.Error(), "/mnt/gone") {
			t.Errorf("err = %q, want it to name the directory — the probe runs against "+
				"several mounts and the message is the only thing that says which", err)
		}
	})
}
