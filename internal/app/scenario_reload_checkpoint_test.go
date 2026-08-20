package app_test

import (
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp/nntptest"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestReload_DoesNotReFetchAWrittenButUnackedArticle pins the window #390 is
// about: an article the assembler has WRITTEN but that no barrier has ACKED
// yet.
//
// That window is real and ordinary. An article becomes Emitted when it is
// dispatched and only becomes Done when a barrier acks it durable, so
// everything written since the last checkpoint sits in between. A file's own
// completion acks it — so to hold an article in the window for the length of a
// test, the file must NOT complete: article 0 downloads and is written while
// article 1 is stalled and never arrives.
//
// ReloadDownloader then clears every Emitted bit. Without a checkpoint first,
// article 0 is offered again, and the ONLY reason it is not lost is that the
// assembler discards the redelivery. What is lost is the bandwidth, and — per
// #390 — if the re-fetch fails terminally against the new server set, the
// article is acked permanently failed while its bytes are already on disk,
// inflating failedBytes for a file that is not damaged.
//
// The assertion is the server's own fetch count, not queue state, because it
// is the one observation that cannot be satisfied by an internal bookkeeping
// change: either the article went back on the wire or it did not.
func TestReload_DoesNotReFetchAWrittenButUnackedArticle(t *testing.T) {
	h := newScenarioHarnessWithConns(t, 2)
	h.Start()

	// One file, two articles. Article 1 stalls forever, so the file never
	// completes and article 0 is never acked by a file finalize.
	writtenID, stalledID := randomMsgID(t), randomMsgID(t)
	raw := []byte("written article body!!")
	held := []byte("never arrives here!!!!")
	total := int64(len(raw) + len(held))
	h.server.AddArticle(writtenID, yencMultiPart("held.bin", raw, 1, 2, total))
	h.server.AddArticle(stalledID, yencMultiPart("held.bin", held, 2, 2, total))
	h.InjectFailure(stalledID, nntptest.FailureStall)

	parsed := &nzb.NZB{Files: []nzb.File{{
		Subject: `"held.bin" yEnc (1/2)`,
		Bytes:   total,
		Articles: []nzb.Article{
			{ID: writtenID, Bytes: len(raw), Number: 1},
			{ID: stalledID, Bytes: len(held), Number: 2},
		},
	}}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Name: "reload-unacked"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if err := h.app.Queue().Add(job); err != nil {
		t.Fatalf("Queue.Add: %v", err)
	}

	// Wait until article 0 has actually been fetched. Reloading before it has
	// been written would test nothing: there would be no unacked article in
	// the window at all, and the test would pass against the unfixed code.
	if !h.WaitUntil(10*time.Second, func() bool {
		return h.server.FetchCount(writtenID) >= 1
	}) {
		t.Fatalf("article was never fetched; fetch count = %d", h.server.FetchCount(writtenID))
	}
	if got := h.server.FetchCount(writtenID); got != 1 {
		t.Fatalf("setup: fetch count = %d, want exactly 1 before the reload", got)
	}

	// Reload onto the same server set. The server identity is irrelevant —
	// what matters is that ReloadDownloader clears every Emitted bit.
	if err := h.app.ReloadDownloader([]config.ServerConfig{
		h.server.ServerConfig("scenario", 2),
	}); err != nil {
		t.Fatalf("ReloadDownloader: %v", err)
	}

	// Wait for PROOF that the new downloader has dispatched, rather than for a
	// duration. The stalled article is genuinely unfinished, so it is re-offered
	// and re-fetched on any correct reload — its second fetch is the signal that
	// a dispatch pass has happened and that the assertion below is measuring
	// behaviour rather than winning a race.
	//
	// A sleep here would make this test pass on the unfixed code whenever the
	// machine was slow enough, which is the failure mode that makes a timing
	// assertion worse than none.
	if !h.WaitUntil(10*time.Second, func() bool {
		return h.server.FetchCount(stalledID) >= 2
	}) {
		t.Fatalf("the new downloader never re-dispatched the unfinished article "+
			"(stalled fetch count = %d), so nothing here proves the written one "+
			"was spared rather than merely not reached yet", h.server.FetchCount(stalledID))
	}

	if got := h.server.FetchCount(writtenID); got != 1 {
		t.Errorf("article was fetched %d times, want 1 — ReloadDownloader cleared its "+
			"Emitted bit while the assembler still held it written-but-unacked, so it "+
			"went back on the wire. Its bytes were already on disk; had this re-fetch "+
			"failed terminally it would have been acked permanently failed and charged "+
			"to failedBytes for a file that is not damaged (#390)", got)
	}
}
