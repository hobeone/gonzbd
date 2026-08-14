package assembler

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/storagefault"
)

// newFailingWriteFile builds an openFile whose every write fails with err and
// whose cache is disabled, so each accepted article goes straight through
// writeOne rather than being buffered.
//
// Caching disabled is the configuration that makes the defect worst rather
// than a convenience: with write_cache_size set to 0 every article takes this
// path, so a full volume fails every write while the barrier's Drain finds an
// empty cache, returns no error, and routes no fault at all.
func newFailingWriteFile(t *testing.T, err error) (*openFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "movie.bin")
	fh, ferr := os.Create(path)
	if ferr != nil {
		t.Fatal(ferr)
	}
	t.Cleanup(func() { _ = fh.Close() })

	key := fileKey{jobID: "job1", fileIdx: 0}
	w := newFileWriter(fh, path, key, newWriteCache(0))
	w.writeAt = func([]byte, int64) (int, error) { return 0, err }
	return &openFile{w: w, info: FileInfo{Path: path, ExpectedSize: 4096}, key: key}, path
}

// TestWriteFault_IsNotCountedTowardCompletion pins the half of the defect that
// loses data outright.
//
// acceptArticle logged the fault from FileWriter.Accept and dropped it, and
// handleSuccessArticle had already committed to returning true, so
// processRequest ran f.partsWritten++ regardless. The file therefore reached
// TotalParts and fired OnFileComplete over bytes that never landed — a file
// finalized as complete, holding pre-allocation zeros, at 100% reported
// health.
//
// The return value IS the observable: it is the only thing standing between a
// failed write and the increment, so the assertion is on it rather than on a
// completion callback that a unit-level fixture cannot reach.
func TestWriteFault_IsNotCountedTowardCompletion(t *testing.T) {
	f, _ := newFailingWriteFile(t, syscall.ENOSPC)
	a := New(makeOpts(t.TempDir(), map[string]FileInfo{}), slog.New(slog.DiscardHandler))

	counted := a.handleSuccessArticle(f, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 7, MessageID: "m1",
		Offset: 0, Data: []byte("AAAA"),
	})

	if counted {
		t.Fatal("a write that failed was counted toward file completion. " +
			"processRequest increments partsWritten on this return, so the file " +
			"reaches TotalParts and finalizes over bytes that never reached disk")
	}
}

// TestWriteFault_IsRoutedOutOfTheAssembler pins the other half: the fault has
// to leave the worker at all.
//
// acceptArticle's doc claimed "a storage fault is logged and left to the
// barrier, which is what surfaces it to the job via Stallable". That was not
// true on this path. The barrier only sees a fault that Drain, Sync, Stat or
// Truncate returns, and a write rejected inside Accept never reaches any of
// them — with the cache disabled there is nothing left buffered for a later
// Drain to fail on, so no fault was ever routed and the job was never stalled.
func TestWriteFault_IsRoutedOutOfTheAssembler(t *testing.T) {
	f, path := newFailingWriteFile(t, syscall.ENOSPC)

	var gotJob string
	var gotFile int
	var gotArt int32
	var gotFault *storagefault.Fault
	opts := makeOpts(t.TempDir(), map[string]FileInfo{})
	opts.OnWriteFault = func(jobID string, fileIdx int, artIdx int32, flt *storagefault.Fault) {
		gotJob, gotFile, gotArt, gotFault = jobID, fileIdx, artIdx, flt
	}
	a := New(opts, slog.New(slog.DiscardHandler))

	a.handleSuccessArticle(f, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 7, MessageID: "m1",
		Offset: 0, Data: []byte("AAAA"),
	})

	if gotFault == nil {
		t.Fatal("the write fault never left the assembler. Nothing stalls the job, " +
			"so a full volume keeps accepting articles and reports full health")
	}
	if gotJob != "job1" || gotFile != 0 {
		t.Errorf("fault routed for job %q file %d, want job1/0", gotJob, gotFile)
	}
	if gotArt != 7 {
		t.Errorf("routed article index %d, want 7 — without it the caller cannot "+
			"clear the Emitted bit, and Emitted is not Outstanding: "+
			"ForEachUnfinishedArticle skips it, so the article is never re-dispatched", gotArt)
	}
	if gotFault.Path != path {
		t.Errorf("fault path = %q, want %q (R27)", gotFault.Path, path)
	}
	if gotFault.Op != "write" {
		t.Errorf("fault op = %q, want \"write\"", gotFault.Op)
	}
}

// TestNoteWriteFault_KeepsAnAlreadyClassifiedFault pins the one decision
// noteWriteFault makes on its own.
//
// FileWriter returns a *storagefault.Fault that already carries the op and
// path the failure actually happened on. Re-running Classify over it would
// relabel every one of them "write" against this file's path, so a fault
// raised on a sync or a truncate would be reported as a write fault — R27 asks
// the surfaced reason to name what failed, not merely something nearby.
func TestNoteWriteFault_KeepsAnAlreadyClassifiedFault(t *testing.T) {
	f, path := newFailingWriteFile(t, syscall.ENOSPC)

	var got *storagefault.Fault
	opts := makeOpts(t.TempDir(), map[string]FileInfo{})
	opts.OnWriteFault = func(_ string, _ int, _ int32, flt *storagefault.Fault) { got = flt }
	a := New(opts, slog.New(slog.DiscardHandler))

	// A fault that arrived from elsewhere, naming a different op and path.
	original := storagefault.Classify("sync", "/mnt/other/vol.rar", syscall.EIO)
	a.noteWriteFault(f.info.Path, WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: 3}, original)

	if got == nil {
		t.Fatal("noteWriteFault did not surface the fault")
	}
	if got.Op != "sync" || got.Path != "/mnt/other/vol.rar" {
		t.Errorf("fault was relabelled to %s %q; an already-classified fault must keep "+
			"the op and path it was raised on, not be re-attributed to this file (%s)",
			got.Op, got.Path, path)
	}

	// An unclassified error is the case Classify is actually for.
	got = nil
	a.noteWriteFault(f.info.Path, WriteRequest{JobID: "job1", FileIdx: 0, ArtIdx: 3}, errors.New("bare"))
	if got == nil {
		t.Fatal("a bare error was not classified and surfaced")
	}
	if got.Op != "write" || got.Path != path {
		t.Errorf("bare error classified as %s %q, want write %q", got.Op, got.Path, path)
	}
}
