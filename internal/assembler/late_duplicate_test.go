package assembler

import "testing"

// TestLateDuplicate_ResolvesAnArticleTheWriterNeverAccepted pins the one case
// where dropping a late article strands it.
//
// The tombstone means this file reached TotalParts, so ordinarily every
// article it names is already resolved — accepted and awaiting the barrier's
// ack, or permanently failed. Dropping a redelivery of one of those is right:
// the barrier has the record and this package no longer has the authority.
//
// The exception is an article that was counted toward TotalParts and then
// un-done. FileWriter.fail clears an article from seenDone when its coalesced
// run fails to write, but partsWritten was incremented when the article was
// ACCEPTED and is never decremented — so other articles can carry the file to
// completion while this one holds no state anywhere. Its Emitted bit was
// cleared by the write fault, the downloader re-dispatched it, and the copy
// that comes back lands here.
//
// Dropped, it is resolved in neither direction: not written, not acked, not
// failed, and Emitted again from the re-dispatch — which
// ForEachUnfinishedArticle skips, so nothing re-dispatches it a second time
// and the job never finishes.
func TestLateDuplicate_ResolvesAnArticleTheWriterNeverAccepted(t *testing.T) {
	dir := t.TempDir()

	t.Run("an article the writer never accepted is reported", func(t *testing.T) {
		a := newHelperAssembler()
		var rejected []int32
		a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
			rejected = append(rejected, artIdx)
		}
		f := newHelperFile(t, dir, "stranded.dat", 1000)
		key := f.w.key
		open := map[fileKey]*openFile{key: f}
		completed := map[fileKey]struct{}{key: {}}

		a.processRequest(WriteRequest{
			JobID: key.jobID, FileIdx: key.fileIdx, ArtIdx: 4,
			MessageID: "<never-accepted@x>", Offset: 0, Data: []byte("abcd"),
		}, open, completed, newWriteCache(0))

		if len(rejected) != 1 || rejected[0] != 4 {
			t.Errorf("OnArticleRejected calls = %v, want [4] — the article holds no state "+
				"anywhere: not written, not acked, not failed, and Emitted from its "+
				"re-dispatch, so nothing will ever resolve it", rejected)
		}
	})

	t.Run("a redelivery of an accepted article is still dropped", func(t *testing.T) {
		a := newHelperAssembler()
		var rejected []int32
		a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string) {
			rejected = append(rejected, artIdx)
		}
		f := newHelperFile(t, dir, "benign.dat", 1000)
		key := f.w.key
		f.w.seenDone[4] = struct{}{}
		open := map[fileKey]*openFile{key: f}
		completed := map[fileKey]struct{}{key: {}}

		a.processRequest(WriteRequest{
			JobID: key.jobID, FileIdx: key.fileIdx, ArtIdx: 4,
			MessageID: "<already-here@x>", Offset: 0, Data: []byte("abcd"),
		}, open, completed, newWriteCache(0))

		if len(rejected) != 0 {
			t.Errorf("OnArticleRejected calls = %v, want none — this article was accepted "+
				"and the barrier will ack it; failing it here charges good bytes against "+
				"par2's recovery budget and degrades the job's reported health", rejected)
		}
	})
}
