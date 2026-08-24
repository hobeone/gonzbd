package downloader

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// TestEmitResult_ClearsTheEmittedBitWhenTheResultIsDropped pins the owner of
// the Emitted bit against its own escape hatch.
//
// MarkArticleEmittedByIdx means "a result for this article is on its way to the
// pipeline". Three callers set it and then call emitResult: the
// exhausted-try-list path, the terminal-decode-error path, and the ordinary
// success path. emitResult's send races ctx.Done(), and when cancellation wins
// the result is discarded — so the claim the bit makes becomes false with
// nothing downstream to correct it. ForEachUnfinishedArticle skips a set
// Emitted bit, so the article is never offered again, its file never completes,
// and its job never finalizes.
//
// Before this, only ClearAllEmitted recovered it. That is a bulk repair for an
// invariant the owner failed to maintain, and it cannot tell this article —
// abandoned, nothing written — from one whose bytes are on disk awaiting a
// barrier. Clearing both is #417; clearing neither stalls the job.
func TestEmitResult_ClearsTheEmittedBitWhenTheResultIsDropped(t *testing.T) {
	q := queue.New()
	job := makeJobWithArticles(t, []string{"a@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	var artIdx int32 = -1
	q.ForEachUnfinishedArticle(func(a queue.UnfinishedArticle) bool {
		artIdx = a.ArtIdx
		return false
	})
	if artIdx < 0 {
		t.Fatal("fixture: the job offered no unfinished article")
	}

	// The buffer is filled below, so the send cannot proceed and cancellation
	// is the only ready case. Asking for 0 would NOT do it: New treats a
	// non-positive CompletionsBuffer as "use the default" and allocates 256
	// (downloader.go:302). The send would then succeed, both select cases
	// would be ready, and Go would choose between them at random — a test that
	// passes alone and fails in the package, for the wrong reason either way.
	d := New(q, nil, nil, Options{CompletionsBuffer: 1}, nil)
	d.completions <- &ArticleResult{}

	if err := q.MarkArticleEmittedByIdx(job.ID, artIdx); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}
	if offered := countOffered(q); offered != 0 {
		t.Fatalf("fixture: %d articles offered while one is emitted, want 0", offered)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := &articleRequest{jobID: job.ID, artIdx: artIdx, messageID: "a@h"}
	d.emitResult(ctx, req, "srv", []byte("payload"), 0, 0, nil)

	// The consequence, not the bit: the article must be dispatchable again.
	// Nothing wrote its bytes, so re-fetching costs a fetch and strands
	// nothing.
	if offered := countOffered(q); offered != 1 {
		t.Errorf("%d articles offered after a dropped result, want 1. The emitted bit "+
			"claims a result is in the pipeline, but the result was discarded — so the "+
			"article is skipped for the life of the process and its job never finalizes",
			offered)
	}
}

// TestEmitResult_LeavesTheEmittedBitWhenTheResultIsDelivered is the other half.
// A delivered result keeps its claim, and the bit must survive until the
// barrier that acks the article clears it.
func TestEmitResult_LeavesTheEmittedBitWhenTheResultIsDelivered(t *testing.T) {
	q := queue.New()
	job := makeJobWithArticles(t, []string{"a@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	var artIdx int32 = -1
	q.ForEachUnfinishedArticle(func(a queue.UnfinishedArticle) bool {
		artIdx = a.ArtIdx
		return false
	})

	d := New(q, nil, nil, Options{CompletionsBuffer: 1}, nil)
	if err := q.MarkArticleEmittedByIdx(job.ID, artIdx); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}

	req := &articleRequest{jobID: job.ID, artIdx: artIdx, messageID: "a@h"}
	d.emitResult(t.Context(), req, "srv", []byte("payload"), 0, 0, nil)

	if got := <-d.Completions(); got.ArtIdx != artIdx {
		t.Fatalf("completion carried artidx %d, want %d", got.ArtIdx, artIdx)
	}
	if offered := countOffered(q); offered != 0 {
		t.Errorf("%d articles offered after a DELIVERED result, want 0. Re-dispatching "+
			"an article whose result is in the pipeline duplicates the fetch, and if its "+
			"bytes are already on disk that is the re-fetch #417 exists to stop", offered)
	}
}

// TestEmitResult_ReportsAFailureToClearRatherThanSwallowingIt covers the one
// error the clear does not ignore.
//
// ErrNotFound and ErrJobNotResident are ordinary — a job can leave the queue
// while its results are in flight, and there is nothing to re-dispatch. An
// out-of-range artIdx is neither: it means the caller and the manifest disagree
// about how many articles the job has, and the article it names keeps an
// Emitted bit nothing will clear. Silence there would hide the one case an
// operator could act on (A2).
func TestEmitResult_ReportsAFailureToClearRatherThanSwallowingIt(t *testing.T) {
	q := queue.New()
	job := makeJobWithArticles(t, []string{"a@h"})
	if err := q.Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	var logged bytes.Buffer
	d := New(q, nil, nil, Options{CompletionsBuffer: 1},
		slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	d.completions <- &ArticleResult{}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Out of range for a one-article job, so the clear fails with neither
	// ErrNotFound nor ErrJobNotResident.
	req := &articleRequest{jobID: job.ID, artIdx: 4096, messageID: "a@h"}
	d.emitResult(ctx, req, "srv", nil, 0, 0, nil)

	if !strings.Contains(logged.String(), "clear the emitted bit for a dropped result") {
		t.Errorf("a clear that failed for a real reason was not reported; log was %q",
			logged.String())
	}
}

func countOffered(q *queue.Queue) int {
	var n int
	q.ForEachUnfinishedArticle(func(queue.UnfinishedArticle) bool {
		n++
		return true
	})
	return n
}
