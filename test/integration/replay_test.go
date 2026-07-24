//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

// TestReplay_HistoricalNZB exercises the full download pipeline using a
// real NZB structure with synthetic (fake) article payloads.
//
// Set REPLAY_NZB to the path of a single NZB file to test.
// Set REPLAY_NZB_DIR to a directory of NZBs to test them all.
//
// The harness:
//  1. Parses the NZB to discover all files and segments.
//  2. Generates deterministic fake yEnc payloads (same size as declared).
//  3. Registers them with the mock NNTP server.
//  4. Runs the full pipeline: download → decode → assemble.
//  5. Verifies all files are created on disk and non-empty.
//
// Since the payloads are fake, par2/unrar/7z will fail — the test runs
// with PP=0 (no post-processing) to avoid those failures.
func TestReplay_HistoricalNZB(t *testing.T) {
	nzbPath := os.Getenv("REPLAY_NZB")
	nzbDir := os.Getenv("REPLAY_NZB_DIR")

	if nzbPath == "" && nzbDir == "" {
		t.Skip("neither REPLAY_NZB nor REPLAY_NZB_DIR set")
	}

	var nzbPaths []string
	if nzbPath != "" {
		nzbPaths = append(nzbPaths, nzbPath)
	}
	if nzbDir != "" {
		err := filepath.WalkDir(nzbDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && (strings.HasSuffix(path, ".nzb") || strings.HasSuffix(path, ".nzb.gz")) {
				nzbPaths = append(nzbPaths, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", nzbDir, err)
		}
	}

	if len(nzbPaths) == 0 {
		t.Skip("no NZB files found")
	}

	for _, path := range nzbPaths {
		path := path // capture
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			replayOneNZB(t, path)
		})
	}
}

// replayOneNZB runs the full replay harness for a single NZB file.
func replayOneNZB(t *testing.T, nzbPath string) {
	t.Helper()

	rawNZB, err := os.ReadFile(nzbPath) //nolint:gosec // G304: test code, user-supplied path
	if err != nil {
		t.Fatalf("read NZB: %v", err)
	}

	parsed, err := nzb.Parse(bytes.NewReader(rawNZB))
	if err != nil {
		t.Fatalf("parse NZB %s: %v", nzbPath, err)
	}

	if len(parsed.Files) == 0 {
		t.Skipf("NZB has no files: %s", nzbPath)
	}

	t.Logf("NZB: %s — %d files, %d total segments",
		filepath.Base(nzbPath), len(parsed.Files), countSegments(parsed))

	// Build mock NNTP server with fake articles.
	srv := mocknntp.NewServer(mocknntp.Config{})

	var totalRegistered int
	for fi := range parsed.Files {
		file := &parsed.Files[fi]
		totalParts := len(file.Articles)

		for ai := range file.Articles {
			art := &file.Articles[ai]

			// Generate a deterministic payload of the declared size.
			// Use a smaller size to keep tests fast (32KB max).
			payloadSize := art.Bytes
			if payloadSize <= 0 {
				payloadSize = 1024 // fallback for NZBs with missing sizes
			}
			if payloadSize > 32*1024 {
				payloadSize = 32 * 1024 // cap to keep tests fast
			}
			payload := generateDeterministicPayload(art.ID, payloadSize)

			// Extract filename from subject for yEnc header.
			filename := nzb.ExtractFilenameFromSubject(file.Subject)
			if filename == "" {
				filename = fmt.Sprintf("file_%d", fi)
			}

			// Encode as yEnc. For multi-part files, use yEnc multi-part format.
			var body []byte
			if totalParts == 1 {
				body = mocknntp.EncodeYEnc(filename, payload)
			} else {
				// Calculate offset from previous parts (capped to match our payload sizes).
				offset := int64(ai) * int64(payloadSize)
				totalSize := int64(totalParts) * int64(payloadSize)

				body = mocknntp.EncodeYEncPart(
					filename,
					art.Number,
					totalParts,
					totalSize,
					offset,
					payload,
				)
			}

			srv.AddArticle(art.ID, body)
			totalRegistered++
		}
	}

	t.Logf("registered %d articles with mock server", totalRegistered)

	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	// Don't use t.Cleanup for srv.Close — we close eagerly below.

	// Use os.MkdirTemp instead of t.TempDir() so we can remove the
	// directory eagerly after verification. t.TempDir() defers cleanup
	// until the parent test returns, which means hundreds of parallel
	// subtests keep all their download dirs alive simultaneously.
	downloadDir, err := os.MkdirTemp("", "replay-*")
	if err != nil {
		_ = srv.Close()
		t.Fatalf("create temp dir: %v", err)
	}

	// Build the application manually instead of newTestAppWithDir so we
	// can shut everything down eagerly. newTestAppWithDir uses t.Cleanup
	// which defers until the parent test completes — with hundreds of
	// parallel NZBs that means hundreds of app instances + mock servers
	// + article maps all alive simultaneously, eating RAM.
	db, err := history.Open(t.Context(), filepath.Join(downloadDir, "history.db"))
	if err != nil {
		_ = srv.Close()
		_ = os.RemoveAll(downloadDir)
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)
	appCfg := buildAppConfig(srv.Addr(), downloadDir)
	application, err := app.New(appCfg, repo)
	if err != nil {
		_ = db.Close()
		_ = srv.Close()
		_ = os.RemoveAll(downloadDir)
		t.Fatalf("app.New: %v", err)
	}
	appCtx, appCancel := context.WithCancel(t.Context())
	if err := application.Start(appCtx); err != nil {
		appCancel()
		_ = db.Close()
		_ = srv.Close()
		_ = os.RemoveAll(downloadDir)
		t.Fatalf("app.Start: %v", err)
	}

	// cleanup tears down everything eagerly, freeing RAM and disk.
	cleanup := func() {
		appCancel()
		_ = application.Shutdown()
		_ = db.Close()
		_ = srv.Close()
		srv.ClearArticles() // release the article map
		_ = os.RemoveAll(downloadDir)
	}
	// Ensure cleanup runs even on t.Fatal paths.
	defer cleanup()

	// Add the job with PP=0 (no post-processing) since payloads are fake.
	job := addNZBJob(t, application, rawNZB, filepath.Base(nzbPath))

	// Wait for all files to complete or timeout.
	// Scale timeout with number of segments, with a minimum of 30s.
	timeout := time.Duration(countSegments(parsed)/10+30) * time.Second
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	// Wait for either PostProcComplete or timeout.
	select {
	case ppc := <-application.PostProcComplete():
		if ppc.JobID != job.ID {
			t.Logf("received PostProcComplete for unexpected job %s (wanted %s)", ppc.JobID, job.ID)
		}
		t.Logf("job completed: %s", ppc.JobID)
	case <-ctx.Done():
		// Log what we know about the job state.
		snap := application.Queue().SnapshotJob(job.ID)
		if snap != nil {
			t.Logf("timeout: job status=%s pending=%d remaining=%d",
				snap.Status, snap.Progress().PendingArticles(), snap.Progress().RemainingBytes())
		}
		t.Fatalf("timeout (%v) waiting for job completion", timeout)
	}

	// Verify at least one file was written to disk.
	var filesFound int
	_ = filepath.Walk(downloadDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil //nolint:nilerr // walk error on one path should not abort
		}
		// Skip internal state files.
		base := filepath.Base(path)
		if base == "history.db" || strings.HasSuffix(base, ".json.gz") ||
			strings.HasSuffix(base, ".db-wal") || strings.HasSuffix(base, ".db-shm") {
			return nil
		}
		filesFound++
		if info.Size() == 0 {
			t.Errorf("file %s is empty", path)
		}
		return nil
	})

	t.Logf("replay %s: %d files created on disk", filepath.Base(nzbPath), filesFound)
	if filesFound == 0 {
		t.Errorf("no files created on disk for %s", filepath.Base(nzbPath))
	}
	// cleanup() runs via defer, tearing down app + mock server + temp dir.
}

// TestReplay_ParseCorpus exercises the NZB parser against a directory of
// real-world NZB files. No downloading happens — this tests that the
// parser handles all structural variations without panicking.
//
// Set NZB_CORPUS_DIR to a directory containing NZB files to test.
func TestReplay_ParseCorpus(t *testing.T) {
	corpusDir := os.Getenv("NZB_CORPUS_DIR")
	if corpusDir == "" {
		t.Skip("NZB_CORPUS_DIR not set")
	}

	var total, passed, failed int

	err := filepath.WalkDir(corpusDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries
		}
		if !strings.HasSuffix(path, ".nzb") {
			return nil
		}

		total++
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()

			data, readErr := os.ReadFile(path) //nolint:gosec // G304: test corpus
			if readErr != nil {
				t.Fatalf("read: %v", readErr)
			}

			parsedNZB, parseErr := nzb.Parse(bytes.NewReader(data))
			if parseErr != nil {
				failed++
				t.Errorf("parse failed: %v", parseErr)
				return
			}

			// Validate structural invariants.
			for i := range parsedNZB.Files {
				file := &parsedNZB.Files[i]
				if len(file.Articles) == 0 {
					t.Errorf("file[%d] has no articles (subject: %q)", i, file.Subject)
				}

				// Segment numbers should be > 0.
				for j := range file.Articles {
					if file.Articles[j].Number <= 0 {
						t.Errorf("file[%d].article[%d]: invalid segment number %d",
							i, j, file.Articles[j].Number)
					}
				}

				// Try to extract a filename from the subject.
				name := nzb.ExtractFilenameFromSubject(file.Subject)
				if name == "" {
					t.Logf("file[%d]: could not extract filename from subject: %q", i, file.Subject)
				}
			}
			passed++
		})
		return nil
	})

	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}

	t.Logf("corpus: %d NZBs total, %d parsed OK, %d failed", total, passed, failed)
}

// --- Helpers ---

// generateDeterministicPayload creates a fake but deterministic payload
// of the requested size, seeded by the message ID. This ensures the same
// NZB always produces the same test data.
func generateDeterministicPayload(messageID string, size int) []byte {
	// Use SHA-256 of the message ID as a repeating seed.
	seed := sha256.Sum256([]byte(messageID))

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = seed[i%len(seed)]
	}
	return payload
}

// countSegments returns the total number of segments across all files.
func countSegments(parsed *nzb.NZB) int {
	total := 0
	for i := range parsed.Files {
		total += len(parsed.Files[i].Articles)
	}
	return total
}

// Ensure app import is used (for PostProcComplete type).
var _ app.PostProcComplete
