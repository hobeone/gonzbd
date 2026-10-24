package assembler

import (
	"errors"
	"syscall"
	"testing"
)

// TestDispatchRequest_ReleasesEveryArticleRequestOnce pins WriteRequest.Release:
// the worker calls it exactly once for an article request, whatever became of
// the article. A path that skipped it would leave the downloader's budget
// charged for bytes nothing holds, and enough of them would stop every
// download for good; a path that called it twice is harmless only because the
// pipeline's release is idempotent.
func TestDispatchRequest_ReleasesEveryArticleRequestOnce(t *testing.T) {
	data := func() []byte { return []byte("0123456789") }
	tests := []struct {
		name string
		// setup prepares the worker state; it returns the request to dispatch.
		setup func(t *testing.T, a *Assembler, open map[fileKey]*openFile,
			completed map[fileKey]struct{}) WriteRequest
	}{
		{"written", func(*testing.T, *Assembler, map[fileKey]*openFile, map[fileKey]struct{}) WriteRequest {
			return WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, Data: data()}
		}},
		{"failed fetch", func(*testing.T, *Assembler, map[fileKey]*openFile, map[fileKey]struct{}) WriteRequest {
			return WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, FatalErr: errors.New("gone")}
		}},
		{"offset out of range", func(*testing.T, *Assembler, map[fileKey]*openFile, map[fileKey]struct{}) WriteRequest {
			return WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, Offset: -1, Data: data()}
		}},
		{"write fault", func(t *testing.T, a *Assembler, open map[fileKey]*openFile, _ map[fileKey]struct{}) WriteRequest {
			f := newHelperFile(t, t.TempDir(), "job_0.dat", 0)
			f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.EIO }
			open[fileKey{jobID: "job", fileIdx: 0}] = f
			return WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, Data: data()}
		}},
		{"file cannot be resolved", func(_ *testing.T, a *Assembler, _ map[fileKey]*openFile, _ map[fileKey]struct{}) WriteRequest {
			a.opts.FileInfo = func(string, int) (FileInfo, error) { return FileInfo{}, errors.New("no such file") }
			return WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, Data: data()}
		}},
		{"late duplicate", func(_ *testing.T, _ *Assembler, _ map[fileKey]*openFile, completed map[fileKey]struct{}) WriteRequest {
			completed[fileKey{jobID: "job", fileIdx: 0}] = struct{}{}
			return WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, Data: data()}
		}},
		{"closed job", func(_ *testing.T, a *Assembler, _ map[fileKey]*openFile, _ map[fileKey]struct{}) WriteRequest {
			a.opts.FileInfo = func(string, int) (FileInfo, error) { return FileInfo{}, ErrJobClosed }
			return WriteRequest{JobID: "job", FileIdx: 0, ArtIdx: 1, Data: data()}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			a := newHelperAssembler()
			a.putBuffer = func([]byte) {}
			a.opts.FileInfo = func(string, int) (FileInfo, error) {
				return FileInfo{Dir: dir, Name: "job_0.dat", TotalParts: 2}, nil
			}
			a.opts.OnArticleRejected = func(string, int, int32, string, int64) {}
			a.opts.OnArticlesUnwritten = func(string, int, []int32) {}
			open := map[fileKey]*openFile{}
			completed := map[fileKey]struct{}{}
			t.Cleanup(func() { a.syncAndCloseAll(open) })

			req := tt.setup(t, a, open, completed)
			releases := 0
			req.Release = func() { releases++ }
			a.dispatchRequest(req, open, completed)
			if releases != 1 {
				t.Fatalf("Release called %d times, want exactly 1", releases)
			}
		})
	}
}

// TestDispatchRequest_ADuplicateIsReleasedOnItsOwn: the second copy of an
// article already written is dropped at admission, and its own Release runs,
// separately from the first copy's.
func TestDispatchRequest_ADuplicateIsReleasedOnItsOwn(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	a.putBuffer = func([]byte) {}
	a.opts.FileInfo = func(string, int) (FileInfo, error) {
		return FileInfo{Dir: dir, Name: "job_0.dat", TotalParts: 3}, nil
	}
	open := map[fileKey]*openFile{}
	t.Cleanup(func() { a.syncAndCloseAll(open) })

	releases := 0
	for range 2 {
		a.dispatchRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: 1, Data: []byte("abc"),
			Release: func() { releases++ },
		}, open, map[fileKey]struct{}{})
	}
	if releases != 2 {
		t.Fatalf("Release called %d times for two copies, want 2", releases)
	}
}
