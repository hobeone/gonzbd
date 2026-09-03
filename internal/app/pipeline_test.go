package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/telemetry"
	"github.com/hobeone/gonzbd/internal/types"
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
		// Discriminating cases: network/timeout errors whose message contains NONE
		// of the legacy string markers.
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

// opaqueTimeoutError implements the Timeout() bool interface.
type opaqueTimeoutError struct{}

func (e *opaqueTimeoutError) Error() string { return "operation deadline reached" }
func (e *opaqueTimeoutError) Timeout() bool { return true }

func TestPipeline_HandleFailureResult(t *testing.T) {
	app := newTestApplication(t)

	articles := make([]nzb.Article, 0, 11)
	articles = append(articles, nzb.Article{ID: "a1@x", Bytes: 100, Number: 1})
	for i := range 9 {
		articles = append(articles, nzb.Article{ID: fmt.Sprintf("fail%d@x", i), Bytes: 100, Number: i + 2})
	}
	articles = append(articles, nzb.Article{ID: "filldone@x", Bytes: 100, Number: 11})
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.mkv", Bytes: 1100, Articles: articles},
	}}
	j, hdr, _ := BuildIngestJob(app.config, parsed, "m.nzb", types.FetchOptions{NzbName: "m.nzb"}, nil)
	_ = app.Dispatcher().Add(j, hdr)

	failIDs := make([]string, 0, 9)
	for i := range 9 {
		failIDs = append(failIDs, fmt.Sprintf("fail%d@x", i))
	}
	ackFailed(t, app.Dispatcher(), j.ID(), failIDs...)
	ackDone(t, app.Dispatcher(), j.ID(), "filldone@x")

	a := assembler.New(assembler.Options{
		FileInfo: func(jobID string, fileIdx int) (assembler.FileInfo, error) {
			return assembler.FileInfo{}, nil
		},
	}, nil)

	p := &pipeline{
		log:        slog.Default(),
		dispatcher: app.Dispatcher(),
		assembler:  a,
		fileInfo:   make(map[fileKey]assembler.FileInfo),
	}

	// 1. Test handleFailureResult with a non-retryable error (triggering early abort)
	hopelessFired := false
	p.onJobHopeless = func(jobID string) {
		if jobID == j.ID() {
			hopelessFired = true
		}
	}

	resTerminal := &downloader.ArticleResult{
		JobID:     j.ID(),
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
	if err := j.MarkArticleEmitted(0); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	retriesBefore := telemetry.ArticlesRetried.Value()

	resRetryable := &downloader.ArticleResult{
		JobID:     j.ID(),
		FileIdx:   0,
		MessageID: "a1@x",
		Err:       nntp.ErrTransient,
	}
	p.handleFailureResult(t.Context(), resRetryable)

	gotJob, ok := app.Dispatcher().Job(j.ID())
	if !ok || gotJob == nil {
		t.Fatalf("job %s vanished from the dispatcher after a retryable failure", j.ID())
	}
	if gotJob.Progress().ArticleEmitted(0) {
		t.Error("expected Emitted to be cleared after retryable failure")
	}
	if got := telemetry.ArticlesRetried.Value(); got != retriesBefore+1 {
		t.Errorf("ArticlesRetried = %d, want %d", got, retriesBefore+1)
	}
}

func TestPipeline_HandleSuccessResult(t *testing.T) {
	app := newTestApplication(t)
	parsed := &nzb.NZB{Files: []nzb.File{
		{Subject: "movie.mkv", Bytes: 300, Articles: []nzb.Article{
			{ID: "a1@x", Bytes: 100, Number: 1},
		}},
	}}
	j, hdr, _ := BuildIngestJob(app.config, parsed, "m.nzb", types.FetchOptions{NzbName: "m.nzb"}, nil)
	_ = app.Dispatcher().Add(j, hdr)

	a := assembler.New(assembler.Options{
		FileInfo: func(jobID string, fileIdx int) (assembler.FileInfo, error) {
			return assembler.FileInfo{}, nil
		},
	}, nil)

	p := &pipeline{
		log:        slog.Default(),
		dispatcher: app.Dispatcher(),
		assembler:  a,
		fileInfo:   make(map[fileKey]assembler.FileInfo),
	}

	if err := j.MarkArticleEmitted(0); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	writtenBefore := telemetry.ArticlesWritten.Value()

	resSuccess := &downloader.ArticleResult{
		JobID:      j.ID(),
		FileIdx:    0,
		MessageID:  "a1@x",
		Data:       []byte("some data"),
		ServerName: "news.server.com",
	}
	p.handleSuccessResult(t.Context(), resSuccess)

	gotJob, ok := app.Dispatcher().Job(j.ID())
	if !ok || gotJob == nil {
		t.Fatal("Job returned nil")
	}
	if stats := gotJob.Progress().ServerStats()["news.server.com"]; stats != 9 {
		t.Errorf("ServerStats[news.server.com] = %d, want 9", stats)
	}
	if _, ok := p.fileInfo[fileKey{jobID: j.ID(), fileIdx: 0}]; !ok {
		t.Error("expected fileInfo map to be populated by registerFile")
	}
	if gotJob.Progress().ArticleEmitted(0) {
		t.Error("expected Emitted to be cleared when WriteArticle fails")
	}
	if got := telemetry.ArticlesWritten.Value(); got != writtenBefore {
		t.Errorf("ArticlesWritten = %d, want %d (should not increment when WriteArticle fails)", got, writtenBefore)
	}
}

func TestRegisterFile_ErrorPaths(t *testing.T) {
	t.Parallel()

	newPipeline := func(d *dispatch.Dispatcher) *pipeline {
		return &pipeline{
			log:         slog.Default(),
			dispatcher:  d,
			downloadDir: t.TempDir(),
			fileInfo:    make(map[fileKey]assembler.FileInfo),
		}
	}

	t.Run("job not in the dispatcher", func(t *testing.T) {
		t.Parallel()
		app := newTestApplication(t)
		p := newPipeline(app.Dispatcher())
		err := p.registerFile("no-such-job", 0)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("registerFile = %v, want a not-found error", err)
		}
	})

	t.Run("manifest unavailable", func(t *testing.T) {
		t.Parallel()
		app := newTestApplication(t)
		j, hdr := newBareJob(t, app, "reg-nomanifest", "md5-reg-nomanifest")
		if err := app.Dispatcher().Add(j, hdr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		j.Evict()

		p := newPipeline(app.Dispatcher())
		err := p.registerFile(j.ID(), 0)
		if err == nil {
			t.Fatal("registerFile returned nil with no manifest")
		}
		if !strings.Contains(err.Error(), "manifest") && !strings.Contains(err.Error(), "resident") {
			t.Errorf("registerFile = %v, want manifest/resident error", err)
		}
	})

	t.Run("file index out of range", func(t *testing.T) {
		t.Parallel()
		app := newTestApplication(t)
		j, hdr := newBareJob(t, app, "reg-range", "md5-reg-range")
		if err := app.Dispatcher().Add(j, hdr); err != nil {
			t.Fatalf("Add: %v", err)
		}
		m := mustManifest(t, j)

		for _, idx := range []int{-1, m.NumFiles()} {
			p := newPipeline(app.Dispatcher())
			err := p.registerFile(j.ID(), idx)
			if err == nil || !strings.Contains(err.Error(), "out of range") {
				t.Errorf("registerFile(%d) = %v, want an out-of-range error", idx, err)
			}
		}
	})
}

func TestHandleSuccessResult_AbandonsTheArticleWhenTheJobIsGone(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)

	forbidden := assembler.New(assembler.Options{
		FileInfo: func(string, int) (assembler.FileInfo, error) {
			t.Error("registerFile ran for a job that is not in the dispatcher")
			return assembler.FileInfo{}, errors.New("no such job")
		},
	}, slog.New(slog.DiscardHandler))

	p := &pipeline{
		log:        slog.New(slog.DiscardHandler),
		dispatcher: app.Dispatcher(),
		assembler:  forbidden,
		fileInfo:   make(map[fileKey]assembler.FileInfo),
	}

	p.handleSuccessResult(t.Context(), &downloader.ArticleResult{
		JobID: "gone", FileIdx: 0, ArtIdx: 0, MessageID: "a@x",
		Data: []byte("payload"), ServerName: "s1",
	})
}

func TestHandleSuccessResult_ReturnsTheArticleWhenTheFileCannotBeRegistered(t *testing.T) {
	t.Parallel()
	app := newTestApplication(t)

	disp, j := helperJob(t, app, "regfail", 1, 1)
	if err := j.MarkArticleEmitted(0); err != nil {
		t.Fatalf("MarkArticleEmitted: %v", err)
	}
	p := &pipeline{
		log:        slog.New(slog.DiscardHandler),
		dispatcher: disp,
		assembler: assembler.New(assembler.Options{
			FileInfo: func(string, int) (assembler.FileInfo, error) { return assembler.FileInfo{}, nil },
		}, slog.New(slog.DiscardHandler)),
		fileInfo: make(map[fileKey]assembler.FileInfo),
	}

	// File 9 does not exist in a one-file job, so registerFile fails.
	p.handleSuccessResult(t.Context(), &downloader.ArticleResult{
		JobID: j.ID(), FileIdx: 9, ArtIdx: 0, MessageID: "regfail-f0-a0@x",
		Data: []byte("payload"), ServerName: "s1",
	})

	gotJob, ok := disp.Job(j.ID())
	if !ok {
		t.Fatal("job not found in dispatcher")
	}
	if gotJob.Progress().ArticleEmitted(0) {
		t.Error("the article was left marked Emitted after a registration failure")
	}
}

func TestHandleSuccessResult_RecordsNothingDurableAndReportsTheBytes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	app := newTestApplication(t)
	disp, j := helperJob(t, app, "happy", 1, 2)
	var notedJob string
	var notedBytes int
	p := &pipeline{
		log:         slog.New(slog.DiscardHandler),
		dispatcher:  disp,
		downloadDir: dir,
		assembler: assembler.New(assembler.Options{
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
		JobID: j.ID(), FileIdx: 0, ArtIdx: 0, MessageID: "happy-f0-a0@x",
		Offset: 4096, Data: payload, CRC: 0xC0FFEE, ServerName: "s1",
	})

	if notedJob != j.ID() || notedBytes != len(payload) {
		t.Errorf("reported (%q, %d) to the checkpoint cadence, want (%q, %d)",
			notedJob, notedBytes, j.ID(), len(payload))
	}

	gotJob, ok := disp.Job(j.ID())
	if !ok {
		t.Fatal("job not found in dispatcher")
	}
	if gotJob.Progress().ArticleDone(0) {
		t.Error("the article is Done straight off the pipeline")
	}
}
