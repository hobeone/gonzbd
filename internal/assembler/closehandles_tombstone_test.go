package assembler

import (
	"syscall"
	"testing"
)

// TestCloseJobHandles_DoesNotTombstoneAFileWhoseDrainFailed pins the second
// half of returning articles to Outstanding: they have to be re-writable when
// they come back.
//
// The -2 control arm drains, closes, deletes from open, and marks the file
// completed. Marking a FAILED one completed re-strands the very articles
// releaseFaulted has just freed: the downloader re-dispatches them, and
// processRequest routes each to handleLateDuplicate with open[key] already
// deleted — whose f == nil guard returns before the seenDone/seenFailed check,
// so OnArticleRejected never fires either. The article ends Emitted with no
// resolution, which is the state the release exists to get out of, plus one
// wasted re-fetch.
func TestCloseJobHandles_DoesNotTombstoneAFileWhoseDrainFailed(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()
	a.opts.OnArticlesUnwritten = func(string, int, []int32) {}

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "job_0.dat", 0)
	f.w.wc = wc
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	if !a.handleSuccessArticle(f, WriteRequest{
		JobID: "job", FileIdx: 0, MessageID: "a", Offset: 0, Data: []byte("AAAA"),
	}) {
		t.Fatal("the article was not accepted, so the fixture never buffered it")
	}
	f.w.writeAt = func([]byte, int64) (int, error) { return 0, syscall.ENOSPC }

	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: -2, MessageID: "job"},
		open, completed, map[string]struct{}{}, wc)

	if _, tombstoned := completed[key]; tombstoned {
		t.Error("a file whose close-time drain failed was marked completed. Its " +
			"rolled-back articles have just been returned to Outstanding, and a " +
			"re-dispatched copy now hits handleLateDuplicate with open[key] nil — " +
			"which returns before it can resolve anything, leaving the article " +
			"Emitted with no resolution")
	}
}

// TestCloseJobHandles_StillTombstonesAFileThatClosedCleanly keeps the guard
// above honest: without it, "never tombstone" satisfies the test, and a
// genuinely completed file would then accept late duplicates forever.
func TestCloseJobHandles_StillTombstonesAFileThatClosedCleanly(t *testing.T) {
	dir := t.TempDir()
	a := newHelperAssembler()

	wc := newWriteCache(1 << 20)
	f := newHelperFile(t, dir, "clean_0.dat", 0)
	f.w.wc = wc
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{key: f}
	completed := map[fileKey]struct{}{}

	a.dispatchRequest(
		WriteRequest{JobID: "", FileIdx: -2, MessageID: "job"},
		open, completed, map[string]struct{}{}, wc)

	if _, tombstoned := completed[key]; !tombstoned {
		t.Error("a file that closed cleanly was not marked completed, so a late " +
			"duplicate for it would reopen the file after the job moved on")
	}
}
