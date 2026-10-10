package assembler

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestFaultedIncumbent_TakenOverThenRedeliveryRefused drives one file through
// the assembler's own request path: an article whose write faults, a
// different article claiming the same range, and the faulted article's
// redelivery.
//
// The faulted article is handed back to Outstanding once, at the fault, and
// owns nothing (FileWriter.owned is claimed only after a nil write). The rival
// writes the unowned range and must not resolve the faulted article a second
// time: failing it permanently on top of that OnArticlesUnwritten would give
// one article two dispositions. Its redelivery is then refused against the
// rival's written bytes, which is where it is counted failed, and only that
// redelivery completes the file.
func TestFaultedIncumbent_TakenOverThenRedeliveryRefused(t *testing.T) {
	a := newHelperAssembler()
	path := filepath.Join(t.TempDir(), "takeover.dat")
	key := fileKey{jobID: "job", fileIdx: 0}
	a.opts.FileInfo = func(string, int) (FileInfo, error) {
		return FileInfo{Path: path, TotalParts: 3}, nil
	}

	var unwritten, rejected []int32
	var faults int
	var completeCalls int
	var bytesAtComplete []byte
	a.opts.OnArticlesUnwritten = func(_ string, _ int, arts []int32) {
		unwritten = append(unwritten, arts...)
	}
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
		rejected = append(rejected, artIdx)
	}
	a.opts.OnWriteFault = func(string, int, *storagefault.Fault) { faults++ }
	a.opts.OnFileComplete = func(string, int) {
		completeCalls++
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read file at completion: %v", err)
		}
		bytesAtComplete = b
	}

	open := map[fileKey]*openFile{}
	completed := map[fileKey]struct{}{}
	send := func(artIdx int32, off int64, payload string) {
		a.processRequest(WriteRequest{
			JobID: "job", FileIdx: 0, ArtIdx: artIdx,
			MessageID: string('a'+artIdx) + "@example",
			Offset:    off, Data: []byte(payload),
		}, open, completed)
	}

	// The neighbour opens the file and is written.
	send(2, 4, "CCCC")
	f := open[key]
	if f == nil {
		t.Fatal("precondition: the first article did not open the file")
	}
	t.Cleanup(func() { _ = f.w.Close() })

	// The incumbent's write faults.
	realWrite := f.w.writeAt
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.EIO }
	send(0, 0, "AAAA")
	f.w.writeAt = realWrite
	if faults != 1 || !slices.Equal(unwritten, []int32{0}) {
		t.Fatalf("after the fault: faults=%d unwritten=%v, want 1 and [0]", faults, unwritten)
	}

	// A different article claims the incumbent's range and lands.
	send(1, 0, "BBBB")
	if len(rejected) != 0 {
		t.Errorf("OnArticleRejected = %v after the takeover, want none: the faulted "+
			"incumbent was already returned to Outstanding, and the rival made no "+
			"claim against it", rejected)
	}
	if !slices.Equal(unwritten, []int32{0}) {
		t.Errorf("OnArticlesUnwritten = %v after the takeover, want [0] — the incumbent "+
			"is reported once, at its fault", unwritten)
	}
	if completeCalls != 0 || f.w.parts() != 2 {
		t.Errorf("after the takeover: parts=%d completions=%d, want 2 and 0 — the "+
			"incumbent holds no part until its redelivery is resolved",
			f.w.parts(), completeCalls)
	}

	// The incumbent comes back from Outstanding and is refused.
	send(0, 0, "AAAA")
	if !slices.Equal(rejected, []int32{0}) {
		t.Errorf("OnArticleRejected = %v after the redelivery, want [0]", rejected)
	}
	if !slices.Equal(unwritten, []int32{0}) {
		t.Errorf("OnArticlesUnwritten = %v after the redelivery, want [0]", unwritten)
	}
	if completeCalls != 1 {
		t.Fatalf("OnFileComplete fired %d times, want 1", completeCalls)
	}
	if string(bytesAtComplete) != "BBBBCCCC" {
		t.Errorf("file at completion = %q, want %q: the rival's bytes over [0,4) and "+
			"the neighbour's over [4,8)", bytesAtComplete, "BBBBCCCC")
	}

	// Any later redelivery moves nothing. The completed file's writer is
	// finished and gone, so the tombstone rejects it again; failing an
	// article already failed is first-writer-wins and changes no state.
	send(0, 0, "AAAA")
	if got, err := os.ReadFile(path); err != nil || string(got) != "BBBBCCCC" {
		t.Errorf("file after a late redelivery = %q (err %v), want BBBBCCCC", got, err)
	}
	if completeCalls != 1 || !slices.Equal(rejected, []int32{0, 0}) {
		t.Errorf("late redelivery: completions=%d rejected=%v, want 1 and [0 0]",
			completeCalls, rejected)
	}
}
