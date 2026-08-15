package assembler

import (
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// TestRelievePressure_RoutesTheFaultAndStopsWriting pins finding 12.
//
// The pressure flush logged each writeOne failure and kept writing the rest
// into the same device. Nothing called Stallable, so on ENOSPC the job was
// never paused and no reason was surfaced, and on a permanent condition it
// never reached Fail — the job kept accepting articles and reporting full
// health while every flush failed.
func TestRelievePressure_RoutesTheFaultAndStopsWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.bin")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fh.Close() })

	key := fileKey{jobID: "job1", fileIdx: 0}
	// A cache small enough that two articles put it under pressure.
	wc := newWriteCache(8)
	w := newFileWriter(fh, path, key, wc)
	var writes int
	w.writeAt = func([]byte, int64) (int, error) {
		writes++
		return 0, syscall.ENOSPC
	}

	var faults []*storagefault.Fault
	opts := makeOpts(dir, map[string]FileInfo{})
	var rolledBack []int32
	opts.OnWriteFault = func(_ string, _ int, f *storagefault.Fault) {
		faults = append(faults, f)
	}
	opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		rolledBack = append(rolledBack, artIdxs...)
	}
	a := New(opts, slog.New(slog.DiscardHandler))

	f := &openFile{w: w, info: FileInfo{Path: path, ExpectedSize: 4096}}
	open := map[fileKey]*openFile{key: f}
	for i := range 3 {
		wc.buffer(key, bufferedArticle{
			offset: int64(i) * 4,
			data:   []byte("AAAA"),
			id:     articleID{msgID: "m", artIdx: int32(i)},
		})
	}

	a.relievePressure(wc, open)

	// Grounding: the flush must have attempted at least one write, or "a
	// fault was routed" would be about a path that never ran.
	if writes == 0 {
		t.Fatal("the pressure flush never wrote, so it cannot have failed")
	}
	if len(faults) == 0 {
		t.Fatal("a full device swallowed every pressure flush without routing a fault. " +
			"The job is never paused, no reason is surfaced, and it goes on accepting " +
			"articles into a device that cannot take them")
	}
	if faults[0].Op != "write" || faults[0].Path != path {
		t.Errorf("fault = %s %q, want write %q (R27)", faults[0].Op, faults[0].Path, path)
	}
	// Every article the flush rolled back, not only the one the write failed
	// on. The rest were never attempted and their buffers are gone, so they
	// are just as un-written — and reporting only the trigger left them
	// neither Done, nor Failed, nor Outstanding.
	if len(rolledBack) < 2 {
		t.Errorf("rolled-back articles = %v, want every article the flush handled; "+
			"the ones after the failing write are stranded with their Emitted bits "+
			"set, and only a restart recovers them", rolledBack)
	}
}

// TestProcessRequest_RoutesAFailedOpen pins finding 11.
//
// processRequest logged openTargetFile's classified fault and returned. The
// file is never inserted into open, so opFiles never lists it, Files() never
// includes it, and the barrier never drains or syncs it — the fault had no
// path to Stallable at all. The pipeline saw WriteArticle return nil, so the
// article stayed Emitted and was permanently skipped. A persistent EACCES or
// EROFS on the download directory left the job at N% with no reason attached,
// which is the outcome openTargetFile's own doc says returning a fault
// prevents.
func TestProcessRequest_RoutesAFailedOpen(t *testing.T) {
	dir := t.TempDir()
	// A resolver that always fails: the file can never be opened, so nothing
	// is ever inserted into the worker's open map.
	opts := makeOpts(dir, map[string]FileInfo{})
	var faults []*storagefault.Fault
	var gotArt int32 = -1
	opts.OnWriteFault = func(_ string, _ int, f *storagefault.Fault) {
		faults = append(faults, f)
	}
	opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		if len(artIdxs) > 0 {
			gotArt = artIdxs[0]
		}
	}

	a := startAssembler(t, opts)
	if err := a.WriteArticle(t.Context(), WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 9, MessageID: "m1",
		Offset: 0, Data: []byte("AAAA"),
	}); err != nil {
		t.Fatalf("WriteArticle: %v", err)
	}
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if len(faults) == 0 {
		t.Fatal("a file that could not be opened routed no fault. It is never in the " +
			"open set, so no barrier operation can ever surface it, and the article " +
			"stays Emitted and is never re-dispatched")
	}
	if gotArt != 9 {
		t.Errorf("routed article %d, want 9 — without it the owner cannot clear the "+
			"Emitted bit and the article is stranded", gotArt)
	}
}
