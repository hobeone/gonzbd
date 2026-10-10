package assembler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// An article's outcome is not an ack (X2): the assembler has no ack authority
// at all. A written article is noted for Sync (FileWriter.unsynced) and
// reported to Options.OnArticleWritten; a failed write is absent from
// unsynced and comes back as a *storagefault.Fault. See filewriter_test.go for
// the base pins this file specializes.

// TestFailedWrites_RetryIsWrittenNotDiscarded pins the half of the failure
// path that is not an ack, end to end.
//
// seenDone means "accepted and counted toward TotalParts". A failed write
// calls w.fail, which deletes from seenDone. The delete is the load-bearing
// half: without it handleSuccessArticle's duplicate branch discards the retry
// (assembler.go, the `if _, dup := w.seenDone[...]` arm) instead of writing it,
// so articles whose writes hit ENOSPC leave the file permanently short, never
// acked and never failed. Silent without par2.
//
// # Why this test drives the Assembler and not the FileWriter
//
// It replaces a version that called w.Accept on a bare FileWriter and asserted
// len(w.seenDone) == 0. That assertion held UNCONDITIONALLY: seenDone is only
// ever written by handleSuccessArticle, so a bare writer's map is empty no
// matter what fail() does. Deleting the seenDone clear left the whole suite
// green. Reaching the map at all requires the real Assembler path.
//
// The assertion is also deliberately the BYTES, not the maps. Asserting
// "seenDone is empty" re-tests fail() against itself; asserting that a retry
// actually lands on disk is the property the maps exist to produce, and it is
// what fails when the clear is dropped.
func TestFailedWrites_RetryIsWrittenNotDiscarded(t *testing.T) {
	const (
		artCount = 6
		artSize  = 100_000
	)

	a := newHelperAssembler()
	dir := t.TempDir()
	path := filepath.Join(dir, "coalesced.dat")
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = fh.Close() })

	key := fileKey{jobID: "job", fileIdx: 0}
	w := newFileWriter(fh, path, key)
	f := &openFile{
		w:    w,
		info: FileInfo{Dir: filepath.Dir(path), Name: filepath.Base(path), ExpectedSize: artCount * artSize},
	}

	failWrites := true
	w.writeAt = func(p []byte, off int64) (int, error) {
		if failWrites {
			return 0, syscall.ENOSPC
		}
		return fh.WriteAt(p, off)
	}

	// Each article is filled with a distinct byte, so a short or mis-ordered
	// file is distinguishable from a merely zero-filled one.
	body := func(i int) []byte {
		b := make([]byte, artSize)
		for j := range b {
			b[j] = byte('A' + i)
		}
		return b
	}
	send := func(i int) {
		a.handleSuccessArticle(f, WriteRequest{
			JobID:     "job",
			MessageID: fmt.Sprintf("msg%d", i),
			ArtIdx:    testArtIdx(i),
			Offset:    int64(i) * artSize,
			Data:      body(i),
		})
	}

	var rolledBack []int32
	a.opts.OnArticlesUnwritten = func(_ string, _ int, artIdxs []int32) {
		rolledBack = append(rolledBack, artIdxs...)
	}

	// Phase 1 — every write hits ENOSPC.
	for i := range artCount {
		send(i)
	}
	if len(w.seenDone) != 0 {
		t.Errorf("seenDone still holds %d articles after their writes failed; a retry "+
			"would be discarded as a duplicate over bytes that are not on disk",
			len(w.seenDone))
	}
	// Rolled back, NOT recorded failed. A failed write is a storage condition
	// and says nothing about the article's availability (A1), so it comes back
	// — and recording it failed made its redelivery take the
	// "already counted as failed" branch, which writes the bytes without
	// counting them and leaves the file's part total permanently short.
	if len(w.seenFailed) != 0 {
		t.Errorf("seenFailed holds %d articles; a storage fault must not resolve "+
			"against the article", len(w.seenFailed))
	}
	if len(rolledBack) != artCount {
		t.Errorf("rolled-back articles = %v, want all %d — the ones not reported are "+
			"stranded with their Emitted bits set, neither Done nor Failed nor "+
			"Outstanding, and only a restart recovers them", rolledBack, artCount)
	}

	// Phase 2 — the device recovers and every article is re-delivered.
	failWrites = false
	for i := range artCount {
		send(i)
	}

	// Phase 3 — the bytes are the assertion.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != artCount*artSize {
		t.Fatalf("file is %d bytes, want %d; the retried articles were discarded as "+
			"duplicates and their bytes never reached disk", len(got), artCount*artSize)
	}
	for i := range artCount {
		want := byte('A' + i)
		seg := got[i*artSize : (i+1)*artSize]
		for j, b := range seg {
			if b != want {
				t.Fatalf("article %d byte %d = %q, want %q; the retry did not write",
					i, j, b, want)
			}
		}
	}
}

// TestDuplicateSuccessIsNotReAcked pins that a duplicate does not add a second
// entry to unsynced. The dedup decision lives in
// handleSuccessArticle, keyed on the FileWriter's own seenDone map (R12).
func TestDuplicateSuccessIsNotReAcked(t *testing.T) {
	a := newHelperAssembler()
	dir := t.TempDir()
	f := newHelperFile(t, dir, "dup.dat", 0)

	req := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("first copy")}
	if !a.handleSuccessArticle(f, req) {
		t.Fatal("first copy was not accepted")
	}
	if got := f.w.unsynced; len(got) != 1 {
		t.Fatalf("unsynced = %v after the first copy, want exactly one entry", got)
	}

	dup := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("second copy")}
	if a.handleSuccessArticle(f, dup) {
		t.Error("a duplicate must not be counted toward TotalParts")
	}

	if got := f.w.unsynced; len(got) != 1 {
		t.Errorf("unsynced = %v, want exactly one entry — the duplicate must not have "+
			"queued a second write for bytes the first copy already claims", got)
	}
}

// TestDuplicateAtADifferentOffsetIsNotReAcked is the same guard against a
// duplicate that does not agree with the first copy about where the article
// belongs. The dedup is keyed on the Message-ID alone, so a second copy
// reporting a different yEnc offset still hits the duplicate branch and must
// not queue a second write.
func TestDuplicateAtADifferentOffsetIsNotReAcked(t *testing.T) {
	a := newHelperAssembler()
	dir := t.TempDir()
	f := newHelperFile(t, dir, "dupoffset.dat", 0)

	req := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("first copy")}
	if !a.handleSuccessArticle(f, req) {
		t.Fatal("first copy was not accepted")
	}

	// Same Message-ID, different offset, after the first copy was written.
	dup := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 4096, Data: []byte("same article, other offset")}
	if a.handleSuccessArticle(f, dup) {
		t.Error("a duplicate must not be counted toward TotalParts")
	}

	if got := f.w.unsynced; len(got) != 1 {
		t.Errorf("unsynced = %v, want exactly one entry; the duplicate's mismatched "+
			"offset must not queue a second write", got)
	}
}

// TestZeroLengthArticleIsReportedWritten pins that a zero-length article takes
// the same path as any other: the WriteAt is a no-op but the article is still
// noted as written.
func TestZeroLengthArticleIsReportedWritten(t *testing.T) {
	w := newTestFileWriter(t)
	if err := w.Accept(articleID{msgID: "msg0", artIdx: 0}, 0, nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got := w.unsynced; len(got) != 1 || got[0] != 0 {
		t.Errorf("unsynced = %v, want [0] — the write is a no-op but the article "+
			"is still noted as written", got)
	}
}

// TestRetryAfterFailedWriteLandsOnDisk translates
// TestFailArticleMovesTheArticleOutOfSeenDone. The dedup lives in
// handleSuccessArticle, keyed on the FileWriter's seenDone/seenFailed maps.
// A write failure must move the article out of seenDone so its retry is not
// silently discarded as an already-accepted duplicate — it has no bytes on
// disk to be a duplicate of.
func TestRetryAfterFailedWriteLandsOnDisk(t *testing.T) {
	a := newHelperAssembler()
	dir := t.TempDir()
	f := newHelperFile(t, dir, "retry.dat", 0)

	req := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("first")}
	if !a.handleSuccessArticle(f, req) {
		t.Fatal("article was not accepted")
	}
	if _, ok := f.w.seenDone[0]; !ok {
		t.Fatal("precondition: accepting the article should record it in seenDone")
	}

	// Force the write to fail directly, mirroring what a storage fault does.
	f.w.fail(articleID{msgID: "msg0", artIdx: 0})
	if _, stillDone := f.w.seenDone[0]; stillDone {
		t.Error("the failed article is still in seenDone; its retry would take " +
			"the discard branch and never be written")
	}
	if _, failed := f.w.seenFailed[0]; failed {
		t.Error("a storage fault resolved against the article (A1). It also makes the " +
			"retry take the \"already counted as failed\" branch, which writes the bytes " +
			"without counting them and leaves the part total permanently short")
	}

	// The consequence the move exists for: the retry's bytes reach the file.
	//
	// It IS counted again, because the roll-back above gave the count back —
	// see FileWriter.fail, which applies the give-back in the same statement
	// pair that clears seenDone. Counting an article whose bytes are not on
	// disk is what let a file reach TotalParts and finalize over them.
	retry := WriteRequest{JobID: "job", MessageID: "msg0", ArtIdx: 0, Offset: 0, Data: []byte("retry")}
	if !a.handleSuccessArticle(f, retry) {
		t.Error("the retry was not counted toward TotalParts, so the file can never " +
			"reach it: its count was given back when the first attempt was rolled back")
	}
	got, err := os.ReadFile(f.info.Path())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "retry" {
		t.Errorf("file holds %q, want %q; the retry was discarded instead of "+
			"written, which is what leaving the seenDone entry causes", got, "retry")
	}
}

// The test that stood here, TestFailToleratesAnEmptyMessageID, translated
// TestFailArticleWithoutAMessageIDStillAcks and has been folded into
// accounting_test.go's TestFileWriter_FailRollsBackEveryArticlesPart, whose
// table already runs the empty-Message-ID identity alongside a real one. Its
// one distinct assertion — that fail leaves seenFailed alone — moved with it.
//
// The claim it made about seenFailed was also too strong. fail does not write
// seenFailed, but it is not the case that only admitPermanentFailure does:
// failPermanent records there too.

// TestFatalAfterAWrittenArticleCannotRetractIt pins that a FatalErr arriving
// after an article's bytes have landed does not undo them: the assembler has one
// reporter and one direction for an article's outcome, so there is no second
// claim to outrace. A permanent failure produces no report here at all — it goes
// to Job.MarkArticleFailed from the pipeline.
//
// It is demonstrated by driving both events through the real worker in the
// losing order, and reading the file.
func TestFatalAfterAWrittenArticleCannotRetractIt(t *testing.T) {
	dir := t.TempDir()
	files := map[string]FileInfo{}
	path := registerFile(t, dir, files, "job1", 0, 1000) // never completes
	a := startAssembler(t, makeOpts(dir, files))

	// The article's bytes land first.
	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "dup@x",
		Offset: 0, Data: []byte("abcd"),
	}); err != nil {
		t.Fatal(err)
	}
	// Then a permanent failure arrives for the SAME article — the order that
	// used to let a Failed ack overwrite a Done one.
	if err := writeArticle(t.Context(), a, WriteRequest{
		JobID: "job1", FileIdx: 0, ArtIdx: 0, MessageID: "dup@x",
		FatalErr: errFatalProbe,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Quiesce(t.Context()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) < 4 || string(got[:4]) != "abcd" {
		t.Errorf("file starts %q, want %q — a permanent failure retracted bytes that are on disk",
			got[:min(len(got), 4)], "abcd")
	}
}

var errFatalProbe = errors.New("permanent article failure (test)")

// TestSync_ActuallyIssuesTheFsync is the syscall-level pin that the fsync is ON
// THE PATH, and that the articles it covers leave unsynced only AFTER it
// returns nil. Before this test, deleting w.handle.Sync() outright left the
// whole suite green — crash suite included — so the one syscall the close-time
// durability step rests on was silently deletable.
//
// # What it does NOT pin, and what a reader must not conclude
//
// It does not pin fsync-to-platter. No unprivileged test can: dirty page cache
// survives process death, so a SIGKILL cannot distinguish a synced file from an
// unsynced one, and POSIX_FADV_DONTNEED / drop_caches both skip dirty pages.
// Real coverage needs a device-mapper log-writes or flakey target, which needs
// root. See docs/durability-contract.md, Accepted limitations #7, and #363.
//
// So this is "the program calls fsync in the right order", not "the bytes are
// on the platter". The second claim remains unverified in this repository.
func TestSync_ActuallyIssuesTheFsync(t *testing.T) {
	w := newTestFileWriter(t)

	var syncs int
	var unsyncedAtSync int
	w.syncFile = func() error {
		syncs++
		// Captured DURING the syscall: if Sync cleared the set first, the
		// ordering bug would be invisible to an after-the-fact assertion.
		unsyncedAtSync = len(w.unsynced)
		return nil
	}

	w.admitAccepted(0)
	if err := w.Accept(articleID{msgID: "m0", artIdx: 0}, 0, []byte("hello")); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(w.unsynced) == 0 {
		t.Fatal("nothing was noted before Sync; the fixture proves nothing")
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if syncs != 1 {
		t.Errorf("fsync issued %d times, want exactly 1", syncs)
	}
	if unsyncedAtSync == 0 {
		t.Error("unsynced was already empty when fsync was called; it must be " +
			"cleared only AFTER a successful fsync, or a failed sync loses the articles it must roll back")
	}
	if len(w.unsynced) != 0 {
		t.Errorf("unsynced still holds %d after a successful fsync, want 0", len(w.unsynced))
	}

	// A FAILING fsync rolls the article written since back into w.poisoned so
	// it returns to Outstanding (#760): Linux reports a writeback error once
	// and marks the failed pages clean, so nothing written before it can be
	// trusted.
	w.admitAccepted(1)
	if err := w.Accept(articleID{msgID: "m1", artIdx: 1}, 5, []byte("world")); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	w.syncFile = func() error { return syscall.EIO }
	if err := w.Sync(); err == nil {
		t.Fatal("Sync returned nil despite a failing fsync")
	}
	if rolled := w.takePoisoned(); len(rolled) != 1 || rolled[0] != 1 {
		t.Errorf("poisoned after failed Sync = %+v, want [artIdx 1] rolled back to Outstanding", rolled)
	}
}

// TestNewFileWriter_BindsSyncFileToTheRealHandle closes the one level of hole
// the syncFile seam still had.
//
// TestSync_ActuallyIssuesTheFsync pins that Sync
// CALLS w.syncFile in the right order. It says nothing about what syncFile
// holds, because that test installs its own stub. So rebinding
// `w.syncFile = handle.Sync` to `func() error { return nil }` in newFileWriter
// compiled and left the ENTIRE suite green — the same "delete the fsync and
// nothing notices" hole that motivated the seam, one level down.
//
// This is a problem the writeAt seam does not have. Its default binding is
// pinned implicitly by every test that writes through and asserts bytes on
// disk. A successful fsync has no observable effect at all, so nothing pins
// syncFile's binding implicitly and it has to be asserted directly.
//
// The observable used here is failure, not success: with the handle closed, a
// syncFile bound to the real *os.File returns ErrClosed, while any stand-in
// that ignores the handle returns nil. That distinguishes the binding without
// needing to observe a successful fsync, which is not observable.
func TestNewFileWriter_BindsSyncFileToTheRealHandle(t *testing.T) {
	w := newTestFileWriter(t)

	// Control: while the handle is open the real fsync succeeds. Without this,
	// a syncFile that ALWAYS errored would satisfy the assertion below.
	if err := w.syncFile(); err != nil {
		t.Fatalf("syncFile on an open handle = %v, want nil", err)
	}

	if err := w.handle.Close(); err != nil {
		t.Fatalf("close handle: %v", err)
	}
	if err := w.syncFile(); err == nil {
		t.Error("syncFile returned nil after its file handle was closed; it is not " +
			"bound to the real *os.File, so the fsync every ack depends on is not " +
			"actually being issued")
	}
}
