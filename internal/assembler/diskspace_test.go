package assembler

import (
	"context"
	"os"
	"testing"
	"time"
)

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
// a.freeBytes is a per-instance field (not shared/global state), set once
// before checkDiskSpace ever runs on this Assembler and never reassigned
// concurrently — the goroutine that calls checkDiskSpace is spawned after
// the field is set, so Go's happens-before guarantee for goroutine creation
// covers it without needing any additional synchronization.
func TestCheckDiskSpace_SurvivesHungStatfs(t *testing.T) {
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
	a.freeBytes = func(ctx context.Context, _ string) (int64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
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
