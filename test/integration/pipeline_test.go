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

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

const pipelineTimeout = 120 * time.Second

// TestPipeline_Par2VerifyAndUnrar exercises the full pipeline:
// download → assembly → quickcheck/par2 verify → unrar → final move.
//
// Creates a RAR archive + par2 set, serves them via mock NNTP, downloads,
// and verifies the extracted file ends up in completeDir.
func TestPipeline_Par2VerifyAndUnrar(t *testing.T) {
	t.Parallel()
	requireTool(t, "unrar")
	requireTool(t, "par2")

	// Use pre-generated repairme.rar and par2 files.
	srcPayload := []byte("This is a file that will be corrupted and repaired using par2 volumes.")
	wantSHA := sha256.Sum256(srcPayload)

	fixtureDir := t.TempDir()
	copyFixture(t, "repairme.rar", fixtureDir, "repairme.rar")
	copyFixture(t, "recovery.par2", fixtureDir, "recovery.par2")
	copyFixture(t, "recovery.vol0+2.par2", fixtureDir, "recovery.vol0+2.par2")

	// Read fixture files → TestFile entries → mock NNTP articles.
	files := fixtureToTestFiles(t, fixtureDir, 50*1024)
	srv := newMockServerFromFixtures(t, files)

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{
		EnableUnrar: true,
	})
	rawNZB := BuildNZB(files)
	// PP=3 enables repair + unpack + cleanup stages.
	addNZBJobPP(t, a, rawNZB, "rar-par2-test", 3)

	waitForPostProcWithTimeout(t, a, pipelineTimeout)

	// The extracted file should be in the complete directory.
	extractedPath := filepath.Join(completeDir, "rar-par2-test", "repairme.txt")
	verifyFileAtPath(t, extractedPath, wantSHA[:])
}

// TestPipeline_Par2RepairThenUnrar corrupts a byte in the RAR archive
// after creation (simulating download corruption), then lets par2 repair
// it before unrar extracts.
func TestPipeline_Par2RepairThenUnrar(t *testing.T) {
	t.Parallel()
	requireTool(t, "unrar")
	requireTool(t, "par2")

	srcPayload := []byte("This is a file that will be corrupted and repaired using par2 volumes.")
	wantSHA := sha256.Sum256(srcPayload)

	fixtureDir := t.TempDir()
	rarPath := copyFixture(t, "repairme.rar", fixtureDir, "repairme.rar")
	copyFixture(t, "recovery.par2", fixtureDir, "recovery.par2")
	copyFixture(t, "recovery.vol0+2.par2", fixtureDir, "recovery.vol0+2.par2")

	// Corrupt a byte in the RAR archive to test repair.
	rarData, err := os.ReadFile(rarPath) //nolint:gosec // G304: test code
	if err != nil {
		t.Fatalf("read rar: %v", err)
	}
	if len(rarData) <= 100 {
		t.Fatalf("RAR fixture is too small to corrupt at offset 100")
	}
	rarData[100] ^= 0xFF
	if err := os.WriteFile(rarPath, rarData, 0o600); err != nil {
		t.Fatalf("corrupt rar: %v", err)
	}

	// Serve the corrupted RAR + intact par2 files.
	files := fixtureToTestFiles(t, fixtureDir, 50*1024)
	srv := newMockServerFromFixtures(t, files)

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{
		EnableUnrar: true,
	})
	rawNZB := BuildNZB(files)
	addNZBJobPP(t, a, rawNZB, "repair-test", 3)

	waitForPostProcWithTimeout(t, a, pipelineTimeout)

	// Despite corruption, par2 should repair and unrar should extract.
	extractedPath := filepath.Join(completeDir, "repair-test", "repairme.txt")
	verifyFileAtPath(t, extractedPath, wantSHA[:])
}

// TestPipeline_7zExtract exercises the 7z extraction pipeline:
// download → assembly → 7z extract → final move.
func TestPipeline_7zExtract(t *testing.T) {
	t.Parallel()
	requireTool(t, "7z")

	srcPayload := []byte("This is a sample text file to compress in 7z format.")
	wantSHA := sha256.Sum256(srcPayload)

	fixtureDir := t.TempDir()
	copyFixture(t, "sample.7z", fixtureDir, "sample.7z")

	files := fixtureToTestFiles(t, fixtureDir, 50*1024)
	srv := newMockServerFromFixtures(t, files)

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{
		Enable7zip: true,
	})
	rawNZB := BuildNZB(files)
	addNZBJobPP(t, a, rawNZB, "7z-test", 3)

	waitForPostProcWithTimeout(t, a, pipelineTimeout)

	extractedPath := filepath.Join(completeDir, "7z-test", "sample.txt")
	verifyFileAtPath(t, extractedPath, wantSHA[:])
}

// TestPipeline_MissingTool_Graceful verifies that when a required extraction
// tool is not found, the job fails gracefully (not panics or hangs) and
// files remain accessible for retry.
func TestPipeline_MissingTool_Graceful(t *testing.T) {
	t.Parallel()

	fixtureDir := t.TempDir()
	copyFixture(t, "nounrar.rar", fixtureDir, "nounrar.rar")

	files := fixtureToTestFiles(t, fixtureDir, 50*1024)
	srv := newMockServerFromFixtures(t, files)

	// Use a bogus unrar path so unrar stage fails.
	downloadDir := filepath.Join(t.TempDir(), "incomplete")
	completeDir := filepath.Join(t.TempDir(), "complete")
	adminDir := filepath.Join(t.TempDir(), "admin")
	for _, d := range []string{downloadDir, completeDir, adminDir} {
		os.MkdirAll(d, 0o750) //nolint:errcheck // test setup
	}

	cfg := buildAppConfig(srv.Addr(), downloadDir)
	cfg.With(func(c *config.Config) {
		c.General.CompleteDir = completeDir
		c.General.AdminDir = adminDir
		c.PostProc.EnableUnrar = true
		c.PostProc.UnrarCommand = "/bin/false"
		c.PostProc.UseGoRAR = false
		c.PostProc.GoRarFallback = false
	})

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)

	a, err := app.New(cfg, repo)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	if err := a.Start(ctx); err != nil {
		cancel()
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = a.Shutdown()
		db.Close()
	})

	rawNZB := BuildNZB(files)
	// PP=3 so the unpack stage actually runs (and fails).
	addNZBJobPP(t, a, rawNZB, "missing-tool", 3)

	// Job should complete (possibly with failure) — not hang.
	waitForPostProcWithTimeout(t, a, pipelineTimeout)

	// The RAR file should still be accessible somewhere for retry.
	// It may be in downloadDir or completeDir depending on the failure path.
	found := findFile(t, downloadDir, "nounrar.rar")
	if found == "" {
		found = findFile(t, completeDir, "nounrar.rar")
	}
	if found == "" {
		t.Error("RAR file not found in either download or complete dir after tool failure")
	} else {
		t.Logf("RAR file preserved at: %s", found)
	}
}

// TestPipeline_PlainFile_NoPostProc verifies that a plain file (no rar/par2)
// downloads and moves to completeDir without errors.
func TestPipeline_PlainFile_NoPostProc(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("plain file content\n"), 200)
	wantSHA := sha256.Sum256(payload)
	files := []TestFile{
		{Name: "plain-file.bin", Payload: payload},
	}
	srv := newMockServer(t, files)

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{})
	rawNZB := BuildNZB(files)
	addNZBJob(t, a, rawNZB, "plain-test")

	waitForPostProcWithTimeout(t, a, 30*time.Second)

	expectedPath := filepath.Join(completeDir, "plain-test", "plain-file.bin")
	verifyFileAtPath(t, expectedPath, wantSHA[:])
}

// TestPipeline_PRiVATE_FullPipeline combines PRiVATE subject naming with
// RAR extraction to test the full NZB → download → correct filename →
// unrar → final move chain.
func TestPipeline_PRiVATE_FullPipeline(t *testing.T) {
	t.Parallel()
	requireTool(t, "unrar")

	srcPayload := []byte("This is a fake mkv file representing a show episode.")
	wantSHA := sha256.Sum256(srcPayload)

	// Create the RAR archive containing our test file.
	fixtureDir := t.TempDir()
	copyFixture(t, "show.s01e02.rar", fixtureDir, "show.s01e02.rar")

	// Read fixture → TestFiles with specific filenames matching what the
	// PRiVATE subject will declare.
	files := fixtureToTestFiles(t, fixtureDir, 50*1024)

	// Register articles with mock NNTP server.
	srv := mocknntp.NewServer(mocknntp.Config{})
	RegisterArticles(srv, files)
	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{
		EnableUnrar: true,
	})

	// Build NZB with PRiVATE subject format.
	rawNZB := BuildNZBCustomSubject(files, SubjectPRiVATE)
	// PP=3 to enable unpack.
	addNZBJobPP(t, a, rawNZB, "private-pipeline", 3)

	waitForPostProcWithTimeout(t, a, pipelineTimeout)

	// The extracted file should be in completeDir.
	extractedPath := filepath.Join(completeDir, "private-pipeline", "show.s01e02.mkv")
	verifyFileAtPath(t, extractedPath, wantSHA[:])
}

// TestPipeline_SubdirectoryUnrar exercises the full pipeline when RAR archives
// live inside a subdirectory — the common Usenet pattern where NZBs organize
// release files into a release-name directory with par2 files at the root:
//
//	<job>/
//	├── Release-GROUP/
//	│   ├── movie.rar
//	│   └── movie.r00
//	├── obfuscated.par2
//	└── obfuscated.vol00+01.par2
//
// This tests that unpack.Scan recursively finds archives in subdirectories
// (the fix in commit c2a0eba).
func TestPipeline_SubdirectoryUnrar(t *testing.T) {
	t.Parallel()
	requireTool(t, "unrar")
	requireTool(t, "par2")

	// Create a known payload.
	srcPayload := bytes.Repeat([]byte("subdirectory unrar test content\n"), 100)
	wantSHA := sha256.Sum256(srcPayload)

	// Build a fixture with archives in a subdirectory and par2 at root.
	fixtureDir := t.TempDir()

	// Subdirectory for the release files.
	releaseDir := filepath.Join(fixtureDir, "Release-GROUP")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatalf("mkdir release dir: %v", err)
	}

	// Copy pre-generated movie.rar inside the subdirectory, and par2 files at the root.
	copyFixture(t, filepath.Join("movie_set", "Release-GROUP", "movie.rar"), releaseDir, "movie.rar")
	copyFixture(t, filepath.Join("movie_set", "recovery.par2"), fixtureDir, "recovery.par2")
	copyFixture(t, filepath.Join("movie_set", "recovery.vol00+41.par2"), fixtureDir, "recovery.vol00+41.par2")

	// Read fixture recursively — file names will include "Release-GROUP/"
	// prefix, reproducing the real-world subdirectory structure.
	files := fixtureToTestFilesRecursive(t, fixtureDir, 50*1024)
	srv := newMockServerFromFixtures(t, files)

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{
		EnableUnrar: true,
	})
	rawNZB := BuildNZB(files)
	// PP=3 enables repair + unpack + cleanup.
	addNZBJobPP(t, a, rawNZB, "subdir-unrar-test", 3)

	waitForPostProcWithTimeout(t, a, pipelineTimeout)

	// The extracted file should be in completeDir. unrar extracts to the
	// job's download dir (not the subdirectory), so the extracted file
	// lands at the job level.
	extractedPath := filepath.Join(completeDir, "subdir-unrar-test", "movie.mkv")
	verifyFileAtPath(t, extractedPath, wantSHA[:])
}
