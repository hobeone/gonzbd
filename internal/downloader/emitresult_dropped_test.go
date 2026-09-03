package downloader

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/dispatch"
)

// TestEmitResult_ClearsTheEmittedBitWhenTheResultIsDropped pins the owner of
// the Emitted bit against its own escape hatch.
//
// MarkArticleEmitted means "a result for this article is on its way to the
// pipeline". Three callers set it and then call emitResult: the
// exhausted-try-list path, the terminal-decode-error path, and the ordinary
// success path. emitResult's send races ctx.Done(), and when cancellation wins
// the result is discarded — so the claim the bit makes becomes false with
// nothing downstream to correct it.
func TestEmitResult_ClearsTheEmittedBitWhenTheResultIsDropped(t *testing.T) {
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, []string{"a@h"})
	addTestJob(t, disp, j, m)

	artIdx := nextUnfinishedArticle(t, disp, j.ID())

	// The buffer is filled below, so the send cannot proceed and cancellation
	// is the only ready case.
	d := New(disp, nil, nil, Options{CompletionsBuffer: 1}, nil)
	d.completions <- &ArticleResult{}

	if err := j.MarkArticleEmitted(int(artIdx)); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	if offered := countOffered(disp); offered != 0 {
		t.Fatalf("fixture: %d articles offered while one is emitted, want 0", offered)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := &articleRequest{jobID: j.ID(), artIdx: artIdx, messageID: "a@h"}
	d.emitResult(ctx, req, "srv", []byte("payload"), 0, 0, nil)

	// The consequence, not the bit: the article must be dispatchable again.
	// Nothing wrote its bytes, so re-fetching costs a fetch and strands
	// nothing.
	if offered := countOffered(disp); offered != 1 {
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
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, []string{"a@h"})
	addTestJob(t, disp, j, m)

	artIdx := nextUnfinishedArticle(t, disp, j.ID())

	d := New(disp, nil, nil, Options{CompletionsBuffer: 1}, nil)
	if err := j.MarkArticleEmitted(int(artIdx)); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}

	req := &articleRequest{jobID: j.ID(), artIdx: artIdx, messageID: "a@h"}
	d.emitResult(t.Context(), req, "srv", []byte("payload"), 0, 0, nil)

	if got := <-d.Completions(); got.ArtIdx != artIdx {
		t.Fatalf("completion carried artidx %d, want %d", got.ArtIdx, artIdx)
	}
	if offered := countOffered(disp); offered != 0 {
		t.Errorf("%d articles offered after a DELIVERED result, want 0. Re-dispatching "+
			"an article whose result is in the pipeline duplicates the fetch, and if its "+
			"bytes are already on disk that is the re-fetch #417 exists to stop", offered)
	}
}

// TestEmitResult_ReportsAFailureToClearRatherThanSwallowingIt covers the one
// error the clear does not ignore.
func TestEmitResult_ReportsAFailureToClearRatherThanSwallowingIt(t *testing.T) {
	disp := newTestDispatcher(t)
	j, m := makeJobWithArticles(t, []string{"a@h"})
	addTestJob(t, disp, j, m)

	var logged bytes.Buffer
	d := New(disp, nil, nil, Options{CompletionsBuffer: 1},
		slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
	d.completions <- &ArticleResult{}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Out of range for a one-article job, so the clear fails.
	req := &articleRequest{jobID: j.ID(), artIdx: 4096, messageID: "a@h"}
	d.emitResult(ctx, req, "srv", nil, 0, 0, nil)

	if !strings.Contains(logged.String(), "clear the emitted bit for a dropped result") {
		t.Errorf("a clear that failed for a real reason was not reported; log was %q",
			logged.String())
	}
}

func nextUnfinishedArticle(t *testing.T, disp *dispatch.Dispatcher, jobID string) int32 {
	t.Helper()
	j, ok := disp.Job(jobID)
	if !ok {
		t.Fatalf("job %s not found", jobID)
	}
	m, err := j.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	p := j.Progress()
	if p == nil {
		t.Fatalf("Progress is nil")
	}
	for fi := range m.NumFiles() {
		lo, hi := m.FileRange(fi)
		for i := lo; i < hi; i++ {
			if !p.ArticleDone(i) && !p.ArticleEmitted(i) {
				return int32(i)
			}
		}
	}
	t.Fatal("no unfinished article found")
	return -1
}

func countOffered(disp *dispatch.Dispatcher) int {
	var n int
	for _, row := range disp.List() {
		j, ok := disp.Job(row.ID)
		if !ok || !j.Resident() {
			continue
		}
		m, err := j.Manifest()
		if err != nil {
			continue
		}
		p := j.Progress()
		if p == nil {
			continue
		}
		for fi := range m.NumFiles() {
			lo, hi := m.FileRange(fi)
			for i := lo; i < hi; i++ {
				if !p.ArticleDone(i) && !p.ArticleEmitted(i) {
					n++
				}
			}
		}
	}
	return n
}
