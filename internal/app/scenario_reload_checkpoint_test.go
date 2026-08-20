package app_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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
// completion acks it — so to hold an article in the window, the file must not
// complete while the test is looking: article 0 downloads and is written while
// article 1 is stalled and has not arrived.
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
	// Write cache off, so "written" means the bytes are on disk rather than
	// in the assembler's userspace buffer. That is not a convenience: #390's
	// damage is precisely that the bytes ARE already on disk when the article
	// is acked permanently failed, so the cached state is a milder case and
	// pinning it would understate the bug. It is also the only form of
	// "written" a test can observe without a barrier — and a barrier is the
	// thing this test must prove has not run.
	h := newScenarioHarnessWithConfig(t, 2, func(c *config.Config) {
		c.Downloads.WriteCacheSize = 0
	})
	h.Start()

	// One file, two articles. Article 1 is stalled, so the file cannot
	// complete while the test sets up, and article 0 is never acked by a file
	// finalize. nntptest stalls are ONE-SHOT (Scripted.handleBody deletes the
	// injected failure on use), so after the reload article 1 is served
	// normally — which is what lets the job finish rather than wedge the
	// harness on cleanup.
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

	// Wait until article 0's bytes are ON DISK.
	//
	// NOT the server's fetch count: Scripted increments that when the BODY
	// command is received, which is before the body is decoded and long
	// before a pipeline worker pwrites it. Waiting on the fetch count would
	// let the reload run while the article was still queued for write, and
	// the test would then fail against FIXED code — the precondition has to
	// be the thing the fix is about, not a proxy that precedes it.
	// The filename is re-resolved from the yEnc header once a part decodes,
	// so it is read fresh on every poll rather than cached on first sight —
	// caching it pins the subject-derived placeholder and then watches a path
	// nothing will ever write to.
	diskPath := func() string {
		snap := h.app.Queue().SnapshotJob(job.ID)
		if snap == nil {
			return ""
		}
		name := snap.Progress().FileFilename(0)
		if name == "" {
			return ""
		}
		return filepath.Join(h.downloadDir, job.Name, name)
	}
	if !h.WaitUntil(10*time.Second, func() bool {
		p := diskPath()
		if p == "" {
			return false
		}
		onDisk, err := os.ReadFile(p) //nolint:gosec // test-controlled path
		return err == nil && len(onDisk) >= len(raw) && bytes.Equal(onDisk[:len(raw)], raw)
	}) {
		jobDir := filepath.Join(h.downloadDir, job.Name)
		ents, rerr := os.ReadDir(jobDir)
		var names []string
		for _, e := range ents {
			if info, ierr := e.Info(); ierr == nil {
				names = append(names, fmt.Sprintf("%s (%d bytes)", e.Name(), info.Size()))
			} else {
				names = append(names, e.Name())
			}
		}
		t.Fatalf("article 0 never reached disk (last path %q); nothing is in the "+
			"written-but-unacked window and the test would assert nothing.\n"+
			"job dir %q contains %v (readdir err: %v)", diskPath(), jobDir, names, rerr)
	}
	if got := h.server.FetchCount(writtenID); got != 1 {
		t.Fatalf("setup: fetch count = %d, want exactly 1 before the reload", got)
	}

	// The article must still be UNACKED, or the window under test does not
	// exist and a passing assertion below would mean nothing. A periodic
	// barrier would ack it; at the default 30s cadence none should have run
	// in the few ms this takes, but "should" is what silent vacuous passes
	// are made of, so it is checked rather than assumed.
	if runs := h.app.BarrierRuns(); runs != 0 {
		t.Fatalf("a barrier ran (%d) before the reload, so article 0 is already "+
			"acked durable and is no longer in the written-but-unacked window "+
			"this test exists to cover", runs)
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
