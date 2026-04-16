//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
	"github.com/hobeone/sabnzbd-go/test/mocknntp"
)

// addNZBJob parses rawNZB, creates a Job, and adds it to the application queue.
func addNZBJob(t *testing.T, a *app.Application, rawNZB []byte, name string) *queue.Job {
	t.Helper()
	parsed, err := nzb.Parse(bytes.NewReader(rawNZB))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: name + ".nzb", Name: name})
	if err != nil {
		t.Fatalf("queue.NewJob: %v", err)
	}
	if err := a.Queue().Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}
	return job
}

// newMockServer builds and starts a mock NNTP server with articles registered,
// registering cleanup with t.Cleanup.
func newMockServer(t *testing.T, files []TestFile) *mocknntp.Server {
	t.Helper()
	srv := mocknntp.NewServer(mocknntp.Config{})
	RegisterArticles(srv, files)
	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() }) //nolint:errcheck // test cleanup
	return srv
}

// newTestAppWithDir starts an app.Application that writes to downloadDir.
func newTestAppWithDir(t *testing.T, mockAddr, downloadDir string) *app.Application {
	t.Helper()
	cfg := buildAppConfig(mockAddr, downloadDir)
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := a.Start(ctx); err != nil {
		cancel()
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := a.Shutdown(); err != nil {
			t.Logf("app.Shutdown: %v", err)
		}
	})
	return a
}

// TestDownload_SingleFileSinglePart verifies the full download path for a
// small single-article NZB. The mock NNTP server serves one yEnc article;
// the assembled file is written to DownloadDir and its SHA-256 matches.
func TestDownload_SingleFileSinglePart(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("hello integration test\n"), 128) // ~2.8 KB
	files := []TestFile{
		{Name: "single.bin", Payload: payload},
	}
	srv := newMockServer(t, files)

	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)
	rawNZB := BuildNZB(files)
	addNZBJob(t, a, rawNZB, "single-part")

	want := sha256.Sum256(payload)
	waitAndVerifySHA256(t, a, downloadDir, "single.bin", want[:], 30*time.Second)
}

// TestDownload_SingleFileMultiPart verifies assembly of a file split across
// multiple articles (~5 KB parts).
func TestDownload_SingleFileMultiPart(t *testing.T) {
	t.Parallel()

	// ~20 KB payload split into 4 parts of ~5 KB each.
	payload := make([]byte, 20*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	files := []TestFile{
		{Name: "multi.bin", Payload: payload, PartSize: 5 * 1024},
	}
	srv := newMockServer(t, files)

	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)
	rawNZB := BuildNZB(files)
	addNZBJob(t, a, rawNZB, "multi-part")

	want := sha256.Sum256(payload)
	waitAndVerifySHA256(t, a, downloadDir, "multi.bin", want[:], 30*time.Second)
}

// TestDownload_MultiFile verifies that two files in one NZB both download
// and assemble correctly.
func TestDownload_MultiFile(t *testing.T) {
	t.Parallel()

	payloadA := bytes.Repeat([]byte("fileA"), 512)
	payloadB := bytes.Repeat([]byte("fileB"), 512)
	files := []TestFile{
		{Name: "alpha.bin", Payload: payloadA},
		{Name: "beta.bin", Payload: payloadB},
	}
	srv := newMockServer(t, files)

	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)
	rawNZB := BuildNZB(files)
	job := addNZBJob(t, a, rawNZB, "multi-file")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Collect completions until both files are confirmed.
	got := make(map[int]bool)
	for len(got) < 2 {
		select {
		case fc := <-a.FileComplete():
			if fc.JobID != job.ID {
				continue
			}
			got[fc.FileIdx] = true
		case <-ctx.Done():
			t.Fatalf("timeout waiting for file completions; got %d/2", len(got))
		}
	}

	// Both files should exist on disk with the correct content.
	wantA := sha256.Sum256(payloadA)
	wantB := sha256.Sum256(payloadB)
	verifyFileOnDisk(t, downloadDir, "alpha.bin", wantA[:])
	verifyFileOnDisk(t, downloadDir, "beta.bin", wantB[:])
}

// TestDownload_MissingArticle verifies graceful behavior when one article is
// not served by the mock server (returns 430). The downloader should not
// fabricate a completion for an incomplete file.
//
// Behavior note: the current downloader marks an article done=false when the
// server returns 430 after exhausting retries. The file therefore never
// completes assembly, and FileComplete() does not fire. This test asserts
// that FileComplete does NOT fire within a short window, confirming that the
// system does not falsely report success.
func TestDownload_MissingArticle(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("missing article test\n"), 64)
	partSize := len(payload) / 2
	parts := SplitIntoParts(payload, partSize)
	totalSize := int64(len(payload))
	totalParts := len(parts)

	srv := mocknntp.NewServer(mocknntp.Config{})
	// Register only part 1; part 2 is missing and server will return 430.
	mid, body := BuildArticle(parts[0], "missing.bin", 1, totalParts, totalSize, 0)
	srv.AddArticle(mid, body)
	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() }) //nolint:errcheck // test cleanup

	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)
	files := []TestFile{
		{Name: "missing.bin", Payload: payload, PartSize: partSize},
	}
	rawNZB := BuildNZB(files)
	addNZBJob(t, a, rawNZB, "missing-article")

	// Assert FileComplete does NOT fire within 5 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	select {
	case fc := <-a.FileComplete():
		t.Errorf("unexpected FileComplete for missing-article job: jobID=%s fileIdx=%d", fc.JobID, fc.FileIdx)
	case <-ctx.Done():
		// Expected: the file did not complete because an article was missing.
		t.Logf("confirmed: FileComplete did not fire for job with missing article (expected)")
	}
}

// waitAndVerifySHA256 waits for a FileComplete signal, then verifies the
// assembled file has the expected SHA-256 digest.
func waitAndVerifySHA256(t *testing.T, a *app.Application, downloadDir, filename string, wantSHA []byte, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	select {
	case <-a.FileComplete():
		// received; fall through to file verification
	case <-ctx.Done():
		t.Fatalf("timeout (%v) waiting for FileComplete", timeout)
	}

	// Search the download directory tree for the target filename.
	found := findFile(t, downloadDir, filename)
	if found == "" {
		t.Fatalf("assembled file %q not found under %s", filename, downloadDir)
	}

	data, err := os.ReadFile(found) //nolint:gosec // G304: test code, path under TempDir
	if err != nil {
		t.Fatalf("read assembled file: %v", err)
	}

	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], wantSHA) {
		t.Errorf("SHA-256 mismatch for %s: got %x, want %x", filename, got, wantSHA)
	}
}

// verifyFileOnDisk checks that filename exists under dir and has the expected
// SHA-256.
func verifyFileOnDisk(t *testing.T, dir, filename string, wantSHA []byte) {
	t.Helper()
	found := findFile(t, dir, filename)
	if found == "" {
		t.Errorf("file %q not found under %s", filename, dir)
		return
	}
	data, err := os.ReadFile(found) //nolint:gosec // G304: test code, path under TempDir
	if err != nil {
		t.Errorf("read %s: %v", filename, err)
		return
	}
	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], wantSHA) {
		t.Errorf("SHA-256 mismatch for %s: got %x, want %x", filename, got, wantSHA)
	}
}

// findFile walks the tree rooted at dir and returns the first path whose base
// name equals filename, or "".
func findFile(t *testing.T, dir, filename string) string {
	t.Helper()
	var result string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || result != "" {
			return nil //nolint:nilerr // walk error on one path should not abort the whole search
		}
		if !info.IsDir() && filepath.Base(path) == filename {
			result = path
		}
		return nil
	})
	return result
}
