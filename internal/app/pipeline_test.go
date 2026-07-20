package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"syscall"
	"testing"

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

func TestRegisterFile_SeedsInitialWriteCursorFromQueue(t *testing.T) {
	q := queue.New()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.mkv", Bytes: 300, Articles: []nzb.Article{
			{ID: "a1@x", Bytes: 100, Number: 1},
			{ID: "a2@x", Bytes: 100, Number: 2},
			{ID: "a3@x", Bytes: 100, Number: 3},
		}},
	}}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: "m.nzb"}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}
	if err := q.SetFileWriteCursor(job.ID, 0, 4096); err != nil {
		t.Fatal(err)
	}

	p := &pipeline{
		log:         slog.Default(),
		queue:       q,
		downloadDir: t.TempDir(),
		fileInfo:    make(map[fileKey]assembler.FileInfo),
	}
	if err := p.registerFile(job.ID, 0); err != nil {
		t.Fatalf("registerFile: %v", err)
	}
	info := p.fileInfo[fileKey{jobID: job.ID, fileIdx: 0}]
	if info.InitialWriteCursor != 4096 {
		t.Errorf("InitialWriteCursor = %d, want 4096", info.InitialWriteCursor)
	}
}

func TestPipeline_HandleFailureResult(t *testing.T) {
	q := queue.New()
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.mkv", Bytes: 300, Articles: []nzb.Article{
			{ID: "a1@x", Bytes: 100, Number: 1},
		}},
	}}
	job, _ := queue.NewJob(parsed, queue.AddOptions{Filename: "m.nzb"}, fsutil.SanitizeOptions{})
	_ = q.Add(job)

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
	job.ArticlesResolved = 10
	job.ArticlesFailed = 9
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
	if err := q.MarkArticleEmitted(job.ID, "a1@x"); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	retriesBefore := telemetry.ArticlesRetried.Value()

	resRetryable := &downloader.ArticleResult{
		JobID:     job.ID,
		FileIdx:   0,
		MessageID: "a1@x",
		Err:       nntp.ErrTransient,
	}
	p.handleFailureResult(t.Context(), resRetryable)

	if gotJob, _ := q.Get(job.ID); gotJob != nil && gotJob.Files[0].Articles[0].Emitted {
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
	if err := q.MarkArticleEmitted(job.ID, "a1@x"); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
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
	if stats := gotJob.ServerStats["news.server.com"]; stats != 9 {
		t.Errorf("ServerStats[news.server.com] = %d, want 9", stats)
	}
	if _, ok := p.fileInfo[fileKey{jobID: job.ID, fileIdx: 0}]; !ok {
		t.Error("expected fileInfo map to be populated by registerFile")
	}
	if gotJob.Files[0].Articles[0].Emitted {
		t.Error("expected Emitted to be cleared when WriteArticle fails")
	}
	if got := telemetry.ArticlesWritten.Value(); got != writtenBefore {
		t.Errorf("ArticlesWritten = %d, want %d (should not increment when WriteArticle fails)", got, writtenBefore)
	}
}
