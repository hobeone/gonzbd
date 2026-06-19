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
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nntp"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
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
