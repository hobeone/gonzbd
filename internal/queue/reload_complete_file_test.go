package queue

import "testing"

// TestClearAllEmitted_DoesNotStrandPendingWorkOnACompleteFile pins the
// invariant #426 broke: every article the queue counts as pending must be
// reachable by ForEachUnfinishedArticle. An article counted but unreachable is
// work nothing will ever pick up, and it shows in the UI as a job that never
// finishes.
//
// The shape is ordinary, not corrupt. A permanently failed article keeps its
// place in the file's part total (FileWriter.admitPermanentFailure increments
// partsWritten, and failPermanent's doc says why), so a file containing one
// still reaches TotalParts, finalizes short, and is marked Complete. Complete
// alongside a failed article is therefore a state the pipeline produces on its
// own.
//
// resetForReload then clears the failed article's done and failed bits so the
// new downloader can retry it, while ForEachUnfinishedArticle skips the whole
// file for being Complete. The article is outstanding, counted, and
// undispatchable.
func TestClearAllEmitted_DoesNotStrandPendingWorkOnACompleteFile(t *testing.T) {
	t.Parallel()

	q := New()
	if err := q.Add(makeTestJob("j1", 1, 3)); err != nil {
		t.Fatal(err)
	}

	ackDone(t, q, "j1", artID(0, 0))
	ackDone(t, q, "j1", artID(0, 1))
	ackFailed(t, q, "j1", artID(0, 2))

	// What the assembler does once the permanent failure has taken the file to
	// TotalParts: finalize it short and tell the queue it is complete.
	if err := q.MarkFileComplete("j1", 0); err != nil {
		t.Fatalf("MarkFileComplete: %v", err)
	}

	q.ClearAllEmitted(nil)

	var reachable int
	q.ForEachUnfinishedArticle(func(UnfinishedArticle) bool {
		reachable++
		return true
	})

	pending := q.SnapshotJob("j1").Progress().PendingArticles()
	if pending != reachable {
		t.Errorf("pending articles = %d but ForEachUnfinishedArticle reaches %d; "+
			"the difference is work counted against the job that nothing can dispatch",
			pending, reachable)
	}
}

// TestResetForReload_LeavesAFailedArticleAloneWhenItsFileIsComplete pins the
// branch directly, at the layer that owns the decision.
//
// A Complete file has been finalized, truncated to its durable bound and
// tombstoned in the assembler's completed set, which is never cleared for the
// life of the process. Re-fetching one of its articles cannot land: the write
// routes to handleLateDuplicate, the buffer is returned to the pool, and the
// article is failed again. Resetting it buys a wasted fetch and the same end
// state, so the article stays resolved.
//
// This is expressible here, unlike the alternative reloader.go rejects
// ("teach resetForReload to skip an article the writer still holds"), which
// needs knowledge of the writer the queue layer does not have. Complete is
// queue-owned state.
func TestResetForReload_LeavesAFailedArticleAloneWhenItsFileIsComplete(t *testing.T) {
	t.Parallel()

	// Two files, so the assertions below distinguish "skipped because the file
	// is Complete" from "skipped for every file" — a guard keyed on the wrong
	// thing would strand the retry on file 1 as well and still pass a
	// single-file fixture.
	m := newManifest([]JobFile{
		{Bytes: 200, Articles: []JobArticle{{ID: "a0", Bytes: 100}, {ID: "a1", Bytes: 100}}},
		{Bytes: 100, Articles: []JobArticle{{ID: "b0", Bytes: 100}}},
	})
	p := newJobProgress(m)

	p.markFailed(m, 0) // file 0, which will be Complete
	p.markFailed(m, 2) // file 1, which will not be
	p.files[0].Complete = true

	failedBytesBefore := p.failedBytes
	if failedBytesBefore == 0 {
		t.Fatal("fixture: markFailed should have recorded failed bytes")
	}
	fileFailedBefore := p.files[0].FailedBytes

	// The return value is not incidental: ClearAllEmitted deletes exactly the
	// stored rows of the articles this reports true for, so a wrong answer
	// desynchronises memory from the record even when the bits below are right.
	if got := p.resetForReload(m, 0, true); got {
		t.Error("resetForReload reported it cleared an article whose file is Complete")
	}
	if got := p.resetForReload(m, 2, true); !got {
		t.Error("resetForReload reported it did not clear an article whose file is incomplete")
	}

	// The Complete file's article keeps its resolution.
	if !p.done.Get(0) {
		t.Error("Done was cleared on a failed article whose file is Complete")
	}
	if !p.failed.Get(0) {
		t.Error("Failed was cleared on a failed article whose file is Complete")
	}
	if p.files[0].FailedBytes != fileFailedBefore {
		t.Errorf("file 0 FailedBytes = %d, want unchanged %d: the article is still failed, "+
			"so unwinding its bytes would understate the damage",
			p.files[0].FailedBytes, fileFailedBefore)
	}

	// The other file's article is still reset, so the guard has not disabled
	// the retry everywhere.
	if p.done.Get(2) {
		t.Error("Done was not cleared on a failed article whose file is incomplete")
	}
	if p.failed.Get(2) {
		t.Error("Failed was not cleared on a failed article whose file is incomplete")
	}

	// Only file 1's article unwound.
	wantFailedBytes := failedBytesBefore - int64(m.ArticleBytes(2))
	if p.failedBytes != wantFailedBytes {
		t.Errorf("failedBytes = %d, want %d (only the incomplete file's article unwinds)",
			p.failedBytes, wantFailedBytes)
	}

	// Emitted is transient and cleared regardless — it says nothing about
	// whether the article is owed a retry.
	if p.emitted.Get(0) || p.emitted.Get(2) {
		t.Error("Emitted was not cleared")
	}
}
