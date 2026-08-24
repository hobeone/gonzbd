package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/history"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/internal/telemetry"
)

// TestIsRetryableDownloaderError verifies error classification uses the
// stdlib net type hierarchy rather than string matching. This prevents
// fragility from OS/Go version changes to error message text.
func TestIsRetryableDownloaderError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		retryable bool
	}{
		// NNTP sentinels — retryable.
		{"ErrNoArticle", nntp.ErrNoArticle, true},
		{"ErrServerUnavailable", nntp.ErrServerUnavailable, true},
		{"ErrAuthRequired", nntp.ErrAuthRequired, true},
		{"ErrTransient", nntp.ErrTransient, true},
		{"ErrClosed", nntp.ErrClosed, true},
		// I/O and context sentinels — retryable.
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"context.Canceled", context.Canceled, true},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"decoder.ErrCRCMismatch", decoder.ErrCRCMismatch, true},
		// Network errors — retryable via *net.OpError.
		{"net.OpError dial", &net.OpError{Op: "dial", Net: "tcp",
			Err: &net.AddrError{Err: "connection refused", Addr: "localhost:119"}}, true},
		{"net.OpError read ECONNRESET", &net.OpError{Op: "read", Net: "tcp",
			Err: syscall.ECONNRESET}, true},
		// Timeout — retryable via Timeout() bool interface.
		{"os.ErrDeadlineExceeded", os.ErrDeadlineExceeded, true},
		{"net.OpError with timeout inner", &net.OpError{Op: "read", Net: "tcp",
			Err: &testTimeoutError{}}, true},
		// Wrapped retryable errors must still be detected.
		{"wrapped ErrNoArticle", errors.Join(errors.New("outer"), nntp.ErrNoArticle), true},
		{"wrapped net.OpError", errors.Join(errors.New("outer"),
			&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		// Discriminating cases for the d48db10 fix: network/timeout errors whose
		// message contains NONE of the legacy string markers ("dial:",
		// "connection", "i/o timeout"). They are retryable only via the
		// type-based checks; the old string-matching code missed them, so these
		// cases fail if the fix is reverted.
		{"net.OpError opaque message", &net.OpError{Op: "read", Net: "tcp",
			Err: errors.New("socket failure")}, true},
		{"timeout error opaque message", &opaqueTimeoutError{}, true},
		// Terminal errors — NOT retryable.
		{"ErrAuthRejected", nntp.ErrAuthRejected, false},
		{"generic error", errors.New("some internal error"), false},
		{"io.EOF", io.EOF, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryableDownloaderError(tc.err)
			if got != tc.retryable {
				t.Errorf("isRetryableDownloaderError(%v) = %v, want %v", tc.err, got, tc.retryable)
			}
		})
	}
}

// testTimeoutError implements net.Error for testing the Timeout() branch.
type testTimeoutError struct{}

func (e *testTimeoutError) Error() string   { return "i/o timeout" }
func (e *testTimeoutError) Timeout() bool   { return true }
func (e *testTimeoutError) Temporary() bool { return true }

// opaqueTimeoutError implements the Timeout() bool interface but its message
// does NOT contain "i/o timeout" — so only the type-based check (not the legacy
// string-matching) can classify it as retryable.
type opaqueTimeoutError struct{}

func (e *opaqueTimeoutError) Error() string { return "operation deadline reached" }
func (e *opaqueTimeoutError) Timeout() bool { return true }

func TestPipeline_HandleFailureResult(t *testing.T) {
	q := queue.New()
	// a1@x is the article under test (left pending); 9 filler articles are
	// marked failed and 1 filler article is marked done below to reach
	// ArticlesResolved=10/ArticlesFailed=9 via real mutations, driving the
	// same early-abort precondition the test previously set by direct
	// field assignment.
	articles := make([]nzb.Article, 0, 11)
	articles = append(articles, nzb.Article{ID: "a1@x", Bytes: 100, Number: 1})
	for i := range 9 {
		articles = append(articles, nzb.Article{ID: fmt.Sprintf("fail%d@x", i), Bytes: 100, Number: i + 2})
	}
	articles = append(articles, nzb.Article{ID: "filldone@x", Bytes: 100, Number: 11})
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.mkv", Bytes: 1100, Articles: articles},
	}}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Filename: "m.nzb"}, fsutil.SanitizeOptions{})
	_ = q.Add(job)

	failIDs := make([]string, 0, 9)
	for i := range 9 {
		failIDs = append(failIDs, fmt.Sprintf("fail%d@x", i))
	}
	ackFailed(t, q, job.ID, failIDs...)
	ackDone(t, q, job.ID, "filldone@x")

	// Create an assembler (not started, so WriteArticle fails with ErrNotStarted)
	a := assembler.New(assembler.Options{
		FileInfo: func(jobID string, fileIdx int) (assembler.FileInfo, error) {
			return assembler.FileInfo{}, nil
		},
	}, nil)

	p := &pipeline{
		log:       slog.Default(),
		queue:     q,
		assembler: a,
		fileInfo:  make(map[fileKey]assembler.FileInfo),
	}

	// 1. Test handleFailureResult with a non-retryable error (triggering early abort)
	hopelessFired := false
	p.onJobHopeless = func(jobID string) {
		if jobID == job.ID {
			hopelessFired = true
		}
	}

	resTerminal := &downloader.ArticleResult{
		JobID:     job.ID,
		FileIdx:   0,
		MessageID: "a1@x",
		Err:       downloader.ErrNoServersLeft,
		Subject:   "movie.mkv",
	}
	p.handleFailureResult(t.Context(), resTerminal)
	if !hopelessFired {
		t.Error("expected onJobHopeless to be called for early abort")
	}

	// 2. Test handleFailureResult with a retryable error
	if err := q.MarkArticleEmittedByIdx(job.ID, artIdxFor(t, q, job.ID, "a1@x")); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}
	retriesBefore := telemetry.ArticlesRetried.Value()

	resRetryable := &downloader.ArticleResult{
		JobID:     job.ID,
		FileIdx:   0,
		MessageID: "a1@x",
		Err:       nntp.ErrTransient,
	}
	p.handleFailureResult(t.Context(), resRetryable)

	if gotJob, _ := q.Get(job.ID); gotJob != nil && gotJob.Progress().ArticleEmitted(0) {
		t.Error("expected Emitted to be cleared after retryable failure")
	}
	if got := telemetry.ArticlesRetried.Value(); got != retriesBefore+1 {
		t.Errorf("ArticlesRetried = %d, want %d", got, retriesBefore+1)
	}
}

func TestPipeline_HandleSuccessResult(t *testing.T) {
	q := queue.New()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.mkv", Bytes: 300, Articles: []nzb.Article{
			{ID: "a1@x", Bytes: 100, Number: 1},
		}},
	}}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Filename: "m.nzb"}, fsutil.SanitizeOptions{})
	_ = q.Add(job)

	a := assembler.New(assembler.Options{
		FileInfo: func(jobID string, fileIdx int) (assembler.FileInfo, error) {
			return assembler.FileInfo{}, nil
		},
	}, nil)

	p := &pipeline{
		log:       slog.Default(),
		queue:     q,
		assembler: a,
		fileInfo:  make(map[fileKey]assembler.FileInfo),
	}

	// Test handleSuccessResult
	if err := q.MarkArticleEmittedByIdx(job.ID, artIdxFor(t, q, job.ID, "a1@x")); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}
	writtenBefore := telemetry.ArticlesWritten.Value()

	resSuccess := &downloader.ArticleResult{
		JobID:      job.ID,
		FileIdx:    0,
		MessageID:  "a1@x",
		Data:       []byte("some data"),
		ServerName: "news.server.com",
	}
	p.handleSuccessResult(t.Context(), resSuccess)

	gotJob, _ := q.Get(job.ID)
	if gotJob == nil {
		t.Fatal("Get returned nil")
	}
	if stats := gotJob.Progress().ServerStats()["news.server.com"]; stats != 9 {
		t.Errorf("ServerStats[news.server.com] = %d, want 9", stats)
	}
	if _, ok := p.fileInfo[fileKey{jobID: job.ID, fileIdx: 0}]; !ok {
		t.Error("expected fileInfo map to be populated by registerFile")
	}
	if gotJob.Progress().ArticleEmitted(0) {
		t.Error("expected Emitted to be cleared when WriteArticle fails")
	}
	if got := telemetry.ArticlesWritten.Value(); got != writtenBefore {
		t.Errorf("ArticlesWritten = %d, want %d (should not increment when WriteArticle fails)", got, writtenBefore)
	}
}

// registerFile resolves a job's file into the assembler's FileInfo map, and
// every way that can fail returns an error the caller acts on. None of those
// branches had coverage; the manifest one is new, and it is the reason the
// others are worth pinning at the same time — a nil manifest used to reach
// here as a bare pointer and the check that caught it was a nil comparison
// nothing forced anyone to write.
func TestRegisterFile_ErrorPaths(t *testing.T) {
	t.Parallel()

	newPipeline := func(q *queue.Queue) *pipeline {
		return &pipeline{
			log:         slog.Default(),
			queue:       q,
			downloadDir: t.TempDir(),
			fileInfo:    make(map[fileKey]assembler.FileInfo),
		}
	}

	t.Run("job not in the queue", func(t *testing.T) {
		t.Parallel()
		p := newPipeline(queue.New())
		err := p.registerFile("no-such-job", 0)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("registerFile = %v, want a not-found error", err)
		}
	})

	t.Run("manifest unavailable", func(t *testing.T) {
		t.Parallel()
		// A store-backed queue so pausing evicts the manifest for real, then
		// delete it from disk so the snapshot's hydration attempt fails too.
		dir := t.TempDir()
		db, err := history.Open(t.Context(), filepath.Join(dir, "history.db"))
		if err != nil {
			t.Fatalf("history.Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		repo := history.NewRepository(db)
		q := queue.New(queue.WithStore(queue.NewSQLiteStore(repo.DB(), dir, repo)),
			queue.WithStateDir(dir))

		job := newBareQueueJob(t, "reg-nomanifest", "md5-reg-nomanifest")
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := q.Pause(job.ID); err != nil {
			t.Fatalf("Pause: %v", err)
		}
		if err := os.RemoveAll(filepath.Join(dir, "manifests")); err != nil {
			t.Fatalf("remove manifests: %v", err)
		}

		p := newPipeline(q)
		err = p.registerFile(job.ID, 0)
		if err == nil {
			t.Fatal("registerFile returned nil with no manifest; the assembler would be handed a zero-valued FileInfo")
		}
		if !strings.Contains(err.Error(), "manifest") {
			t.Errorf("registerFile = %v, want the error to name the manifest as the cause", err)
		}
	})

	t.Run("file index out of range", func(t *testing.T) {
		t.Parallel()
		q := queue.New()
		job := newBareQueueJob(t, "reg-range", "md5-reg-range")
		if err := q.Add(job); err != nil {
			t.Fatalf("Add: %v", err)
		}
		m := mustManifest(t, job)

		for _, idx := range []int{-1, m.NumFiles()} {
			p := newPipeline(q)
			err := p.registerFile(job.ID, idx)
			if err == nil || !strings.Contains(err.Error(), "out of range") {
				t.Errorf("registerFile(%d) = %v, want an out-of-range error", idx, err)
			}
		}
	})
}

// TestHandleSuccessResult_AbandonsTheArticleWhenTheJobIsGone pins the three
// early exits, each of which means the same thing: the job left the queue
// between the fetch and this call.
//
// All three must return the article to the dispatch pool rather than dropping
// it, and none may hand it to the assembler — writing into a file whose job no
// longer exists creates bytes nothing will ever clean up.
func TestHandleSuccessResult_AbandonsTheArticleWhenTheJobIsGone(t *testing.T) {
	t.Parallel()

	// A panicking assembler: reaching it on any of these paths is the defect,
	// so it fails loudly rather than being asserted about afterwards.
	forbidden := assembler.New(assembler.Options{
		FileInfo: func(string, int) (assembler.FileInfo, error) {
			t.Error("registerFile ran for a job that is not in the queue")
			return assembler.FileInfo{}, errors.New("no such job")
		},
	}, slog.New(slog.DiscardHandler))

	p := &pipeline{
		log:       slog.New(slog.DiscardHandler),
		queue:     queue.New(),
		assembler: forbidden,
		fileInfo:  make(map[fileKey]assembler.FileInfo),
	}

	// MarkJobStarted is the first thing that fails for an absent job.
	p.handleSuccessResult(t.Context(), &downloader.ArticleResult{
		JobID: "gone", FileIdx: 0, ArtIdx: 0, MessageID: "a@x",
		Data: []byte("payload"), ServerName: "s1",
	})
}

// TestHandleSuccessResult_ReturnsTheArticleWhenTheFileCannotBeRegistered pins
// the third early exit specifically, because it is the one that can happen to
// a job that IS still present: a file index the manifest does not have.
func TestHandleSuccessResult_ReturnsTheArticleWhenTheFileCannotBeRegistered(t *testing.T) {
	t.Parallel()

	q, job := helperJob(t, "regfail", 1, 1)
	if err := q.MarkArticleEmittedByIdx(job.ID, 0); err != nil {
		t.Fatalf("MarkArticleEmittedByIdx: %v", err)
	}
	p := &pipeline{
		log:   slog.New(slog.DiscardHandler),
		queue: q,
		assembler: assembler.New(assembler.Options{
			FileInfo: func(string, int) (assembler.FileInfo, error) { return assembler.FileInfo{}, nil },
		}, slog.New(slog.DiscardHandler)),
		fileInfo: make(map[fileKey]assembler.FileInfo),
	}

	// File 9 does not exist in a one-file job, so registerFile fails.
	p.handleSuccessResult(t.Context(), &downloader.ArticleResult{
		JobID: job.ID, FileIdx: 9, ArtIdx: 0, MessageID: "regfail-f0-a0@x",
		Data: []byte("payload"), ServerName: "s1",
	})

	snap := q.SnapshotJob(job.ID)
	if snap.Progress().ArticleEmitted(0) {
		t.Error("the article was left marked Emitted after a registration failure, so " +
			"the dispatcher never offers it again and the job stalls at 99%")
	}
}

// TestHandleSuccessResult_RecordsNothingDurableAndReportsTheBytes pins what
// this function does and, as importantly, what it no longer does.
//
// It used to append a Class A fact here — the article's decoded offset, length
// and CRC — BEFORE handing the bytes to the assembler, with no ordering
// against the write and no way to take it back. That is #389 and #421: an
// article the assembler then rejected held a record with no bytes behind it,
// and a bogus yEnc offset was recorded permanently because the append was
// INSERT OR IGNORE and re-fetching was the exact case it ignored. The pipeline
// now records nothing at all; the barrier records after the fsync.
//
// The byte count the checkpoint cadence's volume bound is measured in stays,
// and it is in DECODED bytes — the payload's length, not the NZB's encoded
// figure — because it is compared against a budget describing bytes on disk.
//
// The article's offset and CRC are not lost with the fact: they travel on to
// the assembler, which reports them in its drain, and the barrier records them
// there. That hand-off is pinned in internal/assembler
// (TestSyncTargetFor_RoundTripsThroughTheWorker and the durable-ack suite);
// what is pinned here is that this site claims nothing.
func TestHandleSuccessResult_RecordsNothingDurableAndReportsTheBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	q, job := helperJob(t, "happy", 1, 2)
	var notedJob string
	var notedBytes int
	p := &pipeline{
		log:         slog.New(slog.DiscardHandler),
		queue:       q,
		downloadDir: dir,
		assembler: assembler.New(assembler.Options{
			// TotalParts 2 against one written article, so the file does not
			// complete and the assembler keeps it open for the drain below.
			FileInfo: func(string, int) (assembler.FileInfo, error) {
				return assembler.FileInfo{TotalParts: 2}, nil
			},
		}, slog.New(slog.DiscardHandler)),
		fileInfo: make(map[fileKey]assembler.FileInfo),
		onArticleWritten: func(jobID string, n int) {
			notedJob, notedBytes = jobID, n
		},
	}
	if err := p.assembler.Start(t.Context()); err != nil {
		t.Fatalf("assembler.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.assembler.Stop() })

	payload := []byte("decoded article bytes")
	p.handleSuccessResult(t.Context(), &downloader.ArticleResult{
		JobID: job.ID, FileIdx: 0, ArtIdx: 0, MessageID: "happy-f0-a0@x",
		Offset: 4096, Data: payload, CRC: 0xC0FFEE, ServerName: "s1",
	})

	if notedJob != job.ID || notedBytes != len(payload) {
		t.Errorf("reported (%q, %d) to the checkpoint cadence, want (%q, %d) — the "+
			"volume bound never fires and a fast link carries a whole interval unacked",
			notedJob, notedBytes, job.ID, len(payload))
	}

	// Nothing was RESOLVED, and that is the half the test's name is about. The
	// article's identity, offset and CRC travel on with it to the assembler,
	// which reports them back in its drain; only the barrier turns that report
	// into a record, and only after the fsync. A pipeline that resolved
	// anything here would be claiming durability nothing has established —
	// which is what appending a Class A fact before the write amounted to.
	if snap := q.SnapshotJob(job.ID); snap.Progress().ArticleDone(0) {
		t.Error("the article is Done straight off the pipeline; nothing has fsynced it, " +
			"so a crash here loses bytes the queue has already stopped asking for")
	}
}
