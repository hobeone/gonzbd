package assembler

import "testing"

// TestLateDuplicate_ResolvesAnArticleTheWriterNeverAccepted pins the one case
// where dropping a late article strands it.
//
// The tombstone means this file reached TotalParts, so ordinarily every
// article it names is already resolved — accepted and written, or permanently
// failed. Reporting a redelivery of one of those costs nothing: the job's
// MarkArticleFailed is a no-op for an article already Done.
//
// The exception is an article that was counted toward TotalParts and then
// un-done. A RolledBack clears an article from seenDone and gives its part
// back when its write fails — so other articles can carry the file to
// completion while this one holds no state anywhere. Its Emitted bit was
// cleared by the write fault, the downloader re-dispatched it, and the copy
// that comes back lands here.
//
// Dropped, it is resolved in neither direction: not written, not acked, not
// failed, and Emitted again from the re-dispatch — which
// ForEachUnfinishedArticle skips, so nothing re-dispatches it a second time
// and the job never finishes.
//
// The file is tombstoned and absent from open, which is the only state
// processRequest reaches handleLateDuplicate in.
func TestLateDuplicate_ResolvesAnArticleTheWriterNeverAccepted(t *testing.T) {
	a := newHelperAssembler()
	var rejected []int32
	a.opts.OnArticleRejected = func(_ string, _ int, artIdx int32, _ string, _ int64) {
		rejected = append(rejected, artIdx)
	}
	key := fileKey{jobID: "job", fileIdx: 0}
	open := map[fileKey]*openFile{}
	completed := map[fileKey]struct{}{key: {}}

	a.processRequest(WriteRequest{
		JobID: key.jobID, FileIdx: key.fileIdx, ArtIdx: 4,
		MessageID: "<never-accepted@x>", Offset: 0, Data: []byte("abcd"),
	}, open, completed)

	if len(rejected) != 1 || rejected[0] != 4 {
		t.Errorf("OnArticleRejected calls = %v, want [4] — the article holds no state "+
			"anywhere: not written, not acked, not failed, and Emitted from its "+
			"re-dispatch, so nothing will ever resolve it", rejected)
	}
	if len(open) != 0 {
		t.Errorf("open = %v after a late article, want empty: a tombstoned file is not reopened", open)
	}
}
