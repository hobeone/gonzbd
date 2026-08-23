package assembler

import (
	"os"
	"path/filepath"
	"testing"
)

// newWrittenFileWriter returns a FileWriter holding one successfully written
// article, ready to be drained.
func newWrittenFileWriter(t *testing.T) *FileWriter {
	t.Helper()
	path := filepath.Join(t.TempDir(), "movie.bin")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fh.Close() })

	key := fileKey{jobID: "job1", fileIdx: 0}
	w := newFileWriter(fh, path, key, newWriteCache(0))
	if err := w.Accept(articleID{msgID: "m1", artIdx: 0}, 0, []byte("AAAA"), 0); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	return w
}

// TestDrainReport_SurvivesAFailureAfterTheSync pins the retention against the
// window it was supposed to cover.
//
// Sync discarded the report the moment the fsync returned, but the barrier's
// order is drain-all, sync-all, build, then Commit and AckDurable. Every
// failure AFTER the fsync — a Stat timeout, a failed RunStore.Commit, an
// AckDurable answering ErrJobNotResident — therefore lost the report exactly as
// it did before the retention existed. The reported field's own doc claimed
// coverage of "the sync, the run commit, the truncate"; only the first was
// real.
//
// Losing it is not losing the bytes, but it is losing the file. Those articles
// keep their bytes on disk and are never acked and never named by a run,
// and a redelivery is dropped by handleSuccessArticle's seenDone check with no
// write and no partsWritten increment — so the file cannot complete for the
// life of the handle.
func TestDrainReport_SurvivesAFailureAfterTheSync(t *testing.T) {
	w := newWrittenFileWriter(t)

	first, err := w.Drain()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Grounding: without a first report there is nothing to lose and the
	// assertion below would hold for the wrong reason.
	if len(first) != 1 {
		t.Fatalf("first Drain returned %d articles, want 1", len(first))
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// The barrier's commit or ack fails here. Nothing tells the writer, which
	// is the whole point: the writer cannot know, so it must keep the report
	// until something confirms the cycle landed.
	second, err := w.Drain()
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second Drain returned %d articles, want the report re-delivered. "+
			"The article's bytes are on disk but it is never acked and never earns a "+
			"durable bit, and a redelivery is dropped as a duplicate, so the file can "+
			"never complete for the life of this handle", len(second))
	}
	if second[0].ArtIdx != first[0].ArtIdx {
		t.Errorf("re-reported article %d, want %d", second[0].ArtIdx, first[0].ArtIdx)
	}
}

// TestDrainReport_IsReleasedOnceTheCycleIsConfirmed pins the other side, which
// is what keeps the retained set bounded: a confirmed cycle must drop its
// report, or every later Drain re-reports every article ever written to the
// file and the barrier's per-cycle commit grows without bound.
func TestDrainReport_IsReleasedOnceTheCycleIsConfirmed(t *testing.T) {
	w := newWrittenFileWriter(t)

	if _, err := w.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	w.Confirm()

	after, err := w.Drain()
	if err != nil {
		t.Fatalf("Drain after Confirm: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("Drain re-reported %d articles after the cycle was confirmed; the "+
			"retained set never shrinks and every checkpoint re-does the whole file",
			len(after))
	}
}

// TestJobSyncTargetConfirm_SwallowsAStoppedAssembler pins that a Confirm which
// cannot reach the worker is not an error anyone has to handle.
//
// It records work that already succeeded — the extent is committed and the
// articles are acked — so there is no recovery a caller could perform. What a
// missed Confirm actually costs is one redundant re-report on the next Drain,
// which R12 requires the apply to absorb. Returning an error here would invent
// a failure path for a cycle that has already landed.
func TestJobSyncTargetConfirm_SwallowsAStoppedAssembler(t *testing.T) {
	dir := t.TempDir()
	files := make(map[string]FileInfo)
	registerFile(t, dir, files, "job1", 0, 1)

	a := startAssembler(t, makeOpts(dir, files))
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// A stopped assembler answers ErrAssemblerStopped to every submit. The
	// requirement is that this returns quietly rather than panicking or
	// blocking — it has no error to return by design.
	tgt := a.SyncTargetFor("job1")
	tgt.Confirm(t.Context(), 0)
}
