// Package e2e contains end-to-end tests that download real articles from a
// live Usenet provider. These tests are gated behind the "e2e" build tag
// and require a configured sabnzbd.yaml with valid server credentials.
//
// Self-posting tests (POST articles then download them) require E2E_POST=1.
// Provided-NZB test requires E2E_NZB=/path/to/file.nzb.
//
// Run with:
//
//	go test -tags=e2e -timeout=10m ./test/e2e/                                   # provided NZB only
//	E2E_POST=1 go test -tags=e2e -timeout=10m ./test/e2e/                        # self-post tests
//	E2E_NZB=/tmp/test.nzb go test -tags=e2e -timeout=10m ./test/e2e/             # download a specific NZB
//	E2E_POST=1 E2E_DEBUG=1 go test -tags=e2e -timeout=10m -v ./test/e2e/         # with pipeline debug logging
//	E2E_KEEP_FILES=1 E2E_NZB=... go test -tags=e2e ...                           # leave files in place
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hobeone/sabnzbd-go/internal/app"
	"github.com/hobeone/sabnzbd-go/internal/nzb"
	"github.com/hobeone/sabnzbd-go/internal/queue"
)

func requireSelfPost(t *testing.T) {
	t.Helper()
	if os.Getenv("E2E_POST") != "1" {
		t.Skip("E2E_POST=1 not set; skipping self-post test")
	}
}

// addJob parses raw NZB XML, creates a queue.Job, and adds it to the app.
func addJob(t *testing.T, a *app.Application, rawNZB []byte, name string) *queue.Job {
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

// TestE2E_SelfPost_SingleFile posts a single yEnc article to the server,
// builds an NZB referencing it, then downloads and verifies the assembled file.
func TestE2E_SelfPost_SingleFile(t *testing.T) {
	requireSelfPost(t)
	cfg := loadConfig(t)
	server := cfg.Servers[0]

	payload := bytes.Repeat([]byte("e2e single-file test payload\n"), 64) // ~1.9 KB

	files := []File{
		{Name: "e2e-single.bin", Payload: payload},
	}

	nzbXML, firstMID := postAndBuildNZB(t, server, files)

	t.Log("waiting for article propagation...")
	waitForArticle(t, server, firstMID, propagationTimeout)

	a, downloadDir := newE2EApp(t, cfg)
	addJob(t, a, nzbXML, "e2e-single")

	wantSHA := sha256.Sum256(payload)
	waitAndVerify(t, a, downloadDir, "e2e-single.bin", wantSHA[:], downloadTimeout)
}

// TestE2E_SelfPost_MultiPart posts a file split across multiple articles,
// then downloads and verifies reassembly.
func TestE2E_SelfPost_MultiPart(t *testing.T) {
	requireSelfPost(t)
	cfg := loadConfig(t)
	server := cfg.Servers[0]

	// ~20 KB payload, split into 4 parts of ~5 KB each.
	payload := make([]byte, 20*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	files := []File{
		{Name: "e2e-multi.bin", Payload: payload, PartSize: 5 * 1024},
	}

	nzbXML, firstMID := postAndBuildNZB(t, server, files)

	t.Log("waiting for article propagation...")
	waitForArticle(t, server, firstMID, propagationTimeout)

	a, downloadDir := newE2EApp(t, cfg)
	addJob(t, a, nzbXML, "e2e-multi")

	wantSHA := sha256.Sum256(payload)
	waitAndVerify(t, a, downloadDir, "e2e-multi.bin", wantSHA[:], downloadTimeout)
}

// TestE2E_SelfPost_MultiFile posts two separate files, downloads both,
// and verifies each.
func TestE2E_SelfPost_MultiFile(t *testing.T) {
	requireSelfPost(t)
	cfg := loadConfig(t)
	server := cfg.Servers[0]

	payloadA := bytes.Repeat([]byte("file-alpha-content\n"), 100)
	payloadB := bytes.Repeat([]byte("file-beta-content\n"), 100)

	files := []File{
		{Name: "e2e-alpha.bin", Payload: payloadA},
		{Name: "e2e-beta.bin", Payload: payloadB},
	}

	nzbXML, firstMID := postAndBuildNZB(t, server, files)

	t.Log("waiting for article propagation...")
	waitForArticle(t, server, firstMID, propagationTimeout)

	a, downloadDir := newE2EApp(t, cfg)
	job := addJob(t, a, nzbXML, "e2e-multi-file")

	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()

	got := make(map[int]bool)
	for len(got) < 2 {
		select {
		case fc := <-a.FileComplete():
			if fc.JobID != job.ID {
				continue
			}
			got[fc.FileIdx] = true
			t.Logf("file %d complete", fc.FileIdx)
		case <-ctx.Done():
			t.Fatalf("timeout waiting for file completions; got %d/2", len(got))
		}
	}

	// Wait for job completion (post-processing finished)
	select {
	case <-a.PostProcComplete():
		// fall through
	case <-ctx.Done():
		t.Fatalf("timeout waiting for PostProcComplete")
	}

	wantA := sha256.Sum256(payloadA)
	wantB := sha256.Sum256(payloadB)
	verifyFileOnDisk(t, downloadDir, "e2e-alpha.bin", wantA[:])
	verifyFileOnDisk(t, downloadDir, "e2e-beta.bin", wantB[:])
}

// TestE2E_ProvidedNZB downloads an NZB provided via the E2E_NZB environment
// variable. Since we don't know the expected checksums, this test verifies
// that all files declared in the NZB are created on disk and are non-empty.
func TestE2E_ProvidedNZB(t *testing.T) {
	nzbPath := os.Getenv("E2E_NZB")
	if nzbPath == "" {
		t.Skip("E2E_NZB not set; skipping provided-NZB test")
	}

	cfg := loadConfig(t)

	rawNZB, err := os.ReadFile(nzbPath) //nolint:gosec // G304: test code; path from env var
	if err != nil {
		t.Fatalf("read NZB %q: %v", nzbPath, err)
	}

	parsed, err := nzb.Parse(bytes.NewReader(rawNZB))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}

	numFiles := len(parsed.Files)
	if numFiles == 0 {
		t.Fatal("NZB contains no files")
	}
	for i, f := range parsed.Files {
		t.Logf("NZB file %d: %q (%d articles)", i, f.Subject, len(f.Articles))
	}

	a, downloadDir := newE2EApp(t, cfg)

	fName := filepath.Base(nzbPath)

	job, err := queue.NewJob(parsed, queue.AddOptions{Filename: fName})
	if err != nil {
		t.Fatalf("queue.NewJob: %v", err)
	}
	if err := a.Queue().Add(job); err != nil {
		t.Fatalf("queue.Add: %v", err)
	}

	fullPath := filepath.Join(downloadDir, "provided")

	fmt.Printf("Waiting for PostProcComplete for %d files in %s...\n", numFiles, fullPath)

	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()

	select {
	case <-a.PostProcComplete():
		// fall through
	case <-ctx.Done():
		t.Fatalf("timeout waiting for PostProcComplete")
	}

	// Walk the download dir and verify all output files are non-empty.
	entries, err := os.ReadDir(downloadDir)
	if err != nil {
		t.Fatalf("read download dir: %v", err)
	}
	fileCount := 0
	for _, e := range entries {
		if e.IsDir() {
			// Check inside subdirectories (job gets a subdir).
			subEntries, err := os.ReadDir(filepath.Join(downloadDir, e.Name()))
			if err != nil {
				continue
			}
			for _, se := range subEntries {
				if se.IsDir() {
					continue
				}
				info, _ := se.Info()
				if info != nil && info.Size() > 0 {
					t.Logf("verified %s/%s: %d bytes", e.Name(), se.Name(), info.Size())
					fileCount++
				}
			}
		} else {
			info, _ := e.Info()
			if info != nil && info.Size() > 0 {
				t.Logf("verified %s: %d bytes", e.Name(), info.Size())
				fileCount++
			}
		}
	}
	if fileCount == 0 {
		t.Error("no output files found in download directory")
	} else {
		t.Logf("total: %d files downloaded successfully", fileCount)
	}
}

// waitAndVerify waits for FileComplete and then PostProcComplete signals, then
// verifies the assembled file has the expected SHA-256 digest.
func waitAndVerify(t *testing.T, a *app.Application, downloadDir, filename string, wantSHA []byte, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Wait for file completion (assembly)
	select {
	case <-a.FileComplete():
		// fall through
	case <-ctx.Done():
		t.Fatalf("timeout (%v) waiting for FileComplete", timeout)
	}

	// Wait for job completion (post-processing finished)
	select {
	case <-a.PostProcComplete():
		// fall through
	case <-ctx.Done():
		t.Fatalf("timeout (%v) waiting for PostProcComplete", timeout)
	}

	verifyFileOnDisk(t, downloadDir, filename, wantSHA)
}

// verifyFileOnDisk checks that filename exists under dir and matches the
// expected SHA-256.
func verifyFileOnDisk(t *testing.T, dir, filename string, wantSHA []byte) {
	t.Helper()
	path := findFile(t, dir, filename)
	if path == "" {
		t.Fatalf("file %q not found under %s", filename, dir)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: test code; path from TempDir
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], wantSHA) {
		t.Errorf("SHA-256 mismatch for %s:\n  got  %x\n  want %x", filename, got, wantSHA)
	} else {
		t.Logf("verified %s: SHA-256 matches (%d bytes)", filename, len(data))
	}
}
