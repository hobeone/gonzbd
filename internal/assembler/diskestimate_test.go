package assembler

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fixedStatfs reports free bytes as Bavail blocks of one byte, and counts its
// calls.
func fixedStatfs(free int64, calls *atomic.Int32) func(string, *syscall.Statfs_t) error {
	return func(_ string, buf *syscall.Statfs_t) error {
		calls.Add(1)
		buf.Bavail = uint64(free) //nolint:gosec // G115: test constant, non-negative
		buf.Bsize = 1
		return nil
	}
}

func probeFree(t *testing.T, p *DiskProbe, dir string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	free, err := p.FreeBytes(ctx, dir)
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	return free
}

// TestDiskProbe_NoteWrittenReducesCachedReading pins #987: inside the TTL the
// cached reading is reduced by the bytes written since it was probed, and a
// fresh probe replaces the estimate with the new reading rather than carrying
// the old subtraction forward.
func TestDiskProbe_NoteWrittenReducesCachedReading(t *testing.T) {
	var calls atomic.Int32
	p := NewDiskProbe(time.Hour) // the cache never expires on its own in this test
	p.statfs = fixedStatfs(1000, &calls)
	dir := t.TempDir()

	if got := probeFree(t, p, dir); got != 1000 {
		t.Fatalf("first reading = %d, want 1000", got)
	}
	p.NoteWritten(dir, 300)
	p.NoteWritten(dir, 200)
	if got := probeFree(t, p, dir); got != 500 {
		t.Errorf("reading after 500 bytes written = %d, want 500 (cached 1000 less 500)", got)
	}
	p.NoteWritten(dir, 700)
	if got := probeFree(t, p, dir); got != 0 {
		t.Errorf("reading after 1200 bytes written = %d, want 0 (floored, never negative)", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("statfs called %d times, want 1: the estimate must not cost a probe", n)
	}

	// Force the entry stale and probe again: the subtraction must reset.
	p.mu.Lock()
	st := p.dirs[dir]
	p.mu.Unlock()
	st.mu.Lock()
	st.fetchedAt = time.Now().Add(-2 * time.Hour)
	st.mu.Unlock()
	p.statfs = fixedStatfs(900, &calls)
	if got := probeFree(t, p, dir); got != 900 {
		t.Errorf("reading after a fresh probe = %d, want 900 (new statfs, subtraction reset)", got)
	}
	if got := probeFree(t, p, dir); got != 900 {
		t.Errorf("cached reading after a fresh probe = %d, want 900", got)
	}
}

// TestDiskProbe_NoteWrittenKeepsBytesWrittenDuringProbe pins that bytes
// written while a statfs is in flight stay counted: the reading may predate
// them, and counting them again only errs toward less free space.
func TestDiskProbe_NoteWrittenKeepsBytesWrittenDuringProbe(t *testing.T) {
	p := NewDiskProbe(time.Hour)
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p.statfs = func(_ string, buf *syscall.Statfs_t) error {
		once.Do(func() { close(entered) })
		<-release
		buf.Bavail = 1000
		buf.Bsize = 1
		return nil
	}

	// Create the entry; its first probe blocks in statfs.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, _ = p.FreeBytes(ctx, dir) // times out; the probe goroutine stays parked
	<-entered

	p.NoteWritten(dir, 400)
	close(release)

	if got := probeFree(t, p, dir); got != 600 {
		t.Errorf("reading = %d, want 600 (1000 less the 400 written during the probe)", got)
	}
}

func TestDiskProbe_NoteWrittenIgnoresUnknownDirAndNonPositive(t *testing.T) {
	var calls atomic.Int32
	p := NewDiskProbe(time.Hour)
	p.statfs = fixedStatfs(1000, &calls)
	dir := t.TempDir()

	p.NoteWritten(dir, 999) // no reading yet: nothing to adjust, and no entry is created
	p.mu.Lock()
	n := len(p.dirs)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("NoteWritten on an unprobed dir created %d entries, want 0", n)
	}
	if got := probeFree(t, p, dir); got != 1000 {
		t.Errorf("first reading = %d, want 1000: bytes written before any reading are in it already", got)
	}
	p.NoteWritten(dir, 0)
	p.NoteWritten(dir, -5)
	if got := probeFree(t, p, dir); got != 1000 {
		t.Errorf("reading after non-positive notes = %d, want 1000", got)
	}
}

// TestCheckDiskSpace_CountsOwnWritesBeforeTTL is the #987 repro in miniature:
// the statfs reading is fixed and the TTL far longer than the test, so only the
// assembler's own accounting of what it wrote can bring the estimate under the
// floor. 32 four-byte writes put the first check (after write 16, which sees
// the reading before any write) at 150 free and the second (after write 32) at
// 150-16*4 = 86 < 100.
func TestCheckDiskSpace_CountsOwnWritesBeforeTTL(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 100)

	var lowFrees []int64
	var mu sync.Mutex
	opts := makeOpts(dir, files)
	opts.QueueSize = 64
	opts.MinFreeBytes = 100
	opts.OnLowDisk = func(_ string, free int64) {
		mu.Lock()
		lowFrees = append(lowFrees, free)
		mu.Unlock()
	}

	a := New(opts, nil)
	var calls atomic.Int32
	a.diskProbe = NewDiskProbe(time.Hour)
	a.diskProbe.statfs = fixedStatfs(150, &calls)
	if err := a.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := range 2 * diskCheckInterval {
		req := WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: testArtIdx(i), Offset: int64(i * 4), Data: []byte("XXXX")}
		if err := writeArticle(t.Context(), a, req); err != nil {
			t.Fatalf("WriteArticle %d: %v", i, err)
		}
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lowFrees) != 1 || lowFrees[0] != 86 {
		t.Errorf("OnLowDisk free values = %v, want [86]: the second check must see 150 less the 64 bytes written since the reading", lowFrees)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("statfs called %d times, want 1: the trip must come from the estimate, not a re-probe", n)
	}
}
