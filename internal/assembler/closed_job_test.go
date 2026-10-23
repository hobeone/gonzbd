package assembler

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestOpenTargetFile_ClosedJobDropsTheArticle: a resolver answering
// ErrJobClosed means the job no longer accepts writes. The article is dropped
// as a cancelled job's was: no file is created, no storage fault is raised,
// no article is reported unwritten, and the buffer goes back to its owner.
func TestOpenTargetFile_ClosedJobDropsTheArticle(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	path := registerFile(t, dir, files, "job1", 0, 1)

	var closed atomic.Bool
	opts := makeOpts(dir, files)
	inner := opts.FileInfo
	opts.FileInfo = func(jobID string, fileIdx int) (FileInfo, error) {
		if closed.Load() {
			return FileInfo{}, fmt.Errorf("resolve %s: %w", jobID, ErrJobClosed)
		}
		return inner(jobID, fileIdx)
	}
	var faults, unwritten atomic.Int32
	opts.OnWriteFault = func(string, int, *storagefault.Fault) { faults.Add(1) }
	opts.OnArticlesUnwritten = func(string, int, []int32) { unwritten.Add(1) }

	a := startAssembler(t, opts)
	closed.Store(true)
	released := make(chan struct{})
	_ = writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, Offset: 0, Data: []byte("late"),
		Release: func() { close(released) },
	})
	if err := a.Quiesce(t.Context()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) = %v, want not-exist: a closed job's article opened a file nothing will close", path, err)
	}
	if n := faults.Load(); n != 0 {
		t.Errorf("OnWriteFault fired %d times, want 0: a closed job is not a storage fault", n)
	}
	if n := unwritten.Load(); n != 0 {
		t.Errorf("OnArticlesUnwritten fired %d times, want 0: the drop is silent, as the tombstone's was", n)
	}
	select {
	case <-released:
	default:
		t.Error("the dropped article's Release was not called")
	}
}
