//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestPipeline_FlatUnpack verifies that FlatUnpack=true extracts files from
// a RAR that contains subdirectories into a single flat directory.
//
// Fixture: a RAR containing "a/b/c/nested.txt". With FlatUnpack=false
// (default), the extracted file would be at completeDir/job/a/b/c/nested.txt.
// With FlatUnpack=true, it should be at completeDir/job/nested.txt.
func TestPipeline_FlatUnpack(t *testing.T) {
	t.Parallel()
	requireTool(t, "unrar")

	srcPayload := []byte("nested file content")
	wantSHA := sha256.Sum256(srcPayload)

	// Use pre-generated nested.rar containing "a/b/c/nested.txt".
	fixtureDir := t.TempDir()
	copyFixture(t, "nested.rar", fixtureDir, "nested.rar")

	files := fixtureToTestFiles(t, fixtureDir, 50*1024)
	srv := newMockServerFromFixtures(t, files)

	// --- Run with FlatUnpack=true ---
	aFlat, _, completeDirFlat := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{
		EnableUnrar: true,
		FlatUnpack:  true,
	})
	rawNZB := BuildNZB(files)
	addNZBJobPP(t, aFlat, rawNZB, "flat-test", 3)
	waitForPostProcWithTimeout(t, aFlat, pipelineTimeout)

	// With FlatUnpack the file should be directly in the job dir (no subdirs).
	flatPath := filepath.Join(completeDirFlat, "flat-test", "nested.txt")
	verifyFileAtPath(t, flatPath, wantSHA[:])

	// The top-level subdirectory should NOT exist.
	badSubdir := filepath.Join(completeDirFlat, "flat-test", "a")
	if _, err := os.Stat(badSubdir); err == nil {
		t.Errorf("a/ should not exist with FlatUnpack=true, but it does")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error: %v", err)
	}
}

// TestPipeline_FlatUnpack_Disabled is the complementary test: FlatUnpack=false
// (default) preserves subdirectory structure from the archive.
func TestPipeline_FlatUnpack_Disabled(t *testing.T) {
	t.Parallel()
	requireTool(t, "unrar")

	srcPayload := []byte("nested file content")
	wantSHA := sha256.Sum256(srcPayload)

	fixtureDir := t.TempDir()
	copyFixture(t, "nested.rar", fixtureDir, "nested.rar")

	files := fixtureToTestFiles(t, fixtureDir, 50*1024)
	srv := newMockServerFromFixtures(t, files)

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{
		EnableUnrar: true,
		FlatUnpack:  false,
	})
	rawNZB := BuildNZB(files)
	addNZBJobPP(t, a, rawNZB, "struct-test", 3)
	waitForPostProcWithTimeout(t, a, pipelineTimeout)

	// With FlatUnpack=false, the subdirectory structure should be preserved.
	structPath := filepath.Join(completeDir, "struct-test", "a", "b", "c", "nested.txt")
	verifyFileAtPath(t, structPath, wantSHA[:])
}

// TestPipeline_NiceWrapping verifies that the full download → unrar pipeline
// completes successfully when Nice is set, proving the nice prefix doesn't
// break command execution.
func TestPipeline_NiceWrapping(t *testing.T) {
	t.Parallel()
	requireTool(t, "unrar")
	requireTool(t, "nice")

	srcPayload := []byte("nice test content")
	wantSHA := sha256.Sum256(srcPayload)

	fixtureDir := t.TempDir()
	copyFixture(t, "nice-test.rar", fixtureDir, "nice-test.rar")

	files := fixtureToTestFiles(t, fixtureDir, 50*1024)
	srv := newMockServerFromFixtures(t, files)

	a, _, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{
		EnableUnrar: true,
		Nice:        "-n 19",
	})
	rawNZB := BuildNZB(files)
	addNZBJobPP(t, a, rawNZB, "nice-test", 3)
	waitForPostProcWithTimeout(t, a, pipelineTimeout)

	// If nice wrapping breaks command execution, the pipeline will fail
	// and the file won't appear.
	extractedPath := filepath.Join(completeDir, "nice-test", "nice.txt")
	verifyFileAtPath(t, extractedPath, wantSHA[:])
}

// ---------------------------------------------------------------------------
// Fixture helper: RAR with subdirectory structure
// ---------------------------------------------------------------------------

// createRarFixtureWithSubdirs creates a RAR archive that preserves relative
// paths (subdirectories). Unlike createRarFixture which uses -ep (strip path),
// this uses the default path mode so the archive retains directory structure.
func createRarFixtureWithSubdirs(t *testing.T, dir, archiveName string, files map[string][]byte) string {
	t.Helper()
	requireTool(t, "rar")

	// Write source files, creating subdirectories as needed.
	var relPaths []string
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		relPaths = append(relPaths, name)
	}

	archivePath := filepath.Join(dir, archiveName)
	// Use -m0 (store, no compression) and -r (recursive) but NOT -ep,
	// so subdirectory structure is preserved in the archive.
	args := []string{"a", "-m0", archivePath}
	args = append(args, relPaths...)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	//nolint:gosec // G204: test code with controlled inputs
	cmd := exec.CommandContext(ctx, "rar", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rar create %s: %v\noutput: %s", archiveName, err, out)
	}

	// Delete source files so only the archive remains.
	for name := range files {
		os.RemoveAll(filepath.Join(dir, filepath.SplitList(name)[0]))
	}
	// Clean up any subdirectories.
	for name := range files {
		parts := filepath.Dir(name)
		if parts != "." {
			os.RemoveAll(filepath.Join(dir, parts))
		}
	}

	return archivePath
}
