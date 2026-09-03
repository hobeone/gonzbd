//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"log/slog"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/types"
)

// TestNaming_JobDirectoryMatchesName verifies that the download directory
// for a job uses the job name, not some other derivation. The assembled
// file must exist at downloadDir/jobName/filename — not anywhere else.
func TestNaming_JobDirectoryMatchesName(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("naming test payload\n"), 64)
	files := []TestFile{
		{Name: "testfile.bin", Payload: payload},
	}
	srv := newMockServer(t, files)

	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)
	rawNZB := BuildNZB(files)
	addNZBJob(t, a, rawNZB, "my-test-job")

	waitForPostProcWithTimeout(t, a, 30*time.Second)

	// Verify the file exists at the EXACT expected path.
	expectedPath := filepath.Join(downloadDir, "my-test-job", "testfile.bin")
	wantSHA := sha256.Sum256(payload)
	verifyFileAtPath(t, expectedPath, wantSHA[:])
}

// TestNaming_NZBExtensionStripped verifies that .nzb extensions are stripped
// from job names, preventing download directories like "movie.nzb/".
func TestNaming_NZBExtensionStripped(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("nzb ext test\n"), 64)
	files := []TestFile{
		{Name: "testfile.bin", Payload: payload},
	}
	srv := newMockServer(t, files)

	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)
	rawNZB := BuildNZB(files)

	// Simulate what Sonarr/Radarr does: pass nzbname with .nzb extension.
	parsed, err := nzb.Parse(bytes.NewReader(rawNZB))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	j, hdr, err := app.BuildIngestJob(a.Config(), parsed, "My.Movie.2024.nzb", types.FetchOptions{
		NzbName: "My.Movie.2024.nzb",
	}, slog.Default())
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	if err := a.AddJob(t.Context(), j, hdr, rawNZB, false); err != nil {
		t.Fatalf("app.AddJob: %v", err)
	}

	waitForPostProcWithTimeout(t, a, 30*time.Second)

	// The directory should be "My.Movie.2024", NOT "My.Movie.2024.nzb".
	goodPath := filepath.Join(downloadDir, "My.Movie.2024", "testfile.bin")
	badPath := filepath.Join(downloadDir, "My.Movie.2024.nzb")

	wantSHA := sha256.Sum256(payload)
	verifyFileAtPath(t, goodPath, wantSHA[:])
	verifyDirNotExists(t, badPath)
}

// TestNaming_PRiVATESubject verifies that [PRiVATE]-style NZB subjects
// produce correct filenames on disk. The filename should be extracted from
// the third bracket group.
func TestNaming_PRiVATESubject(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("private subject test\n"), 64)
	files := []TestFile{
		{Name: "show.s01e02.720p.web.h264-group.r00", Payload: payload},
	}
	srv := newMockServerFromFixtures(t, files)

	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)

	// Build NZB with PRiVATE subject format.
	rawNZB := BuildNZBCustomSubject(files, SubjectPRiVATE)
	addNZBJob(t, a, rawNZB, "private-test")

	waitForPostProcWithTimeout(t, a, 30*time.Second)

	// The file should be named exactly as the bracket content — no extra
	// extension appended.
	expectedPath := filepath.Join(downloadDir, "private-test", "show.s01e02.720p.web.h264-group.r00")
	wantSHA := sha256.Sum256(payload)
	verifyFileAtPath(t, expectedPath, wantSHA[:])

	// Verify no ".rar" was appended.
	badPath := expectedPath + ".rar"
	if _, err := os.Stat(badPath); err == nil {
		t.Errorf("file %q should not exist — .rar was appended to filename", badPath)
	}
}

// TestNaming_QuotedFilename verifies the standard quoted subject format
// produces the correct filename on disk.
func TestNaming_QuotedFilename(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("quoted filename test\n"), 64)
	files := []TestFile{
		{Name: "My.Movie.2024.BluRay.mkv", Payload: payload},
	}
	srv := newMockServer(t, files)

	downloadDir := t.TempDir()
	a := newTestAppWithDir(t, srv.Addr(), downloadDir)
	rawNZB := BuildNZB(files) // uses SubjectQuoted by default
	addNZBJob(t, a, rawNZB, "quoted-test")

	waitForPostProcWithTimeout(t, a, 30*time.Second)

	expectedPath := filepath.Join(downloadDir, "quoted-test", "My.Movie.2024.BluRay.mkv")
	wantSHA := sha256.Sum256(payload)
	verifyFileAtPath(t, expectedPath, wantSHA[:])
}

// TestNaming_FinalMoveToCompleteDir verifies that completed jobs are moved
// from downloadDir to completeDir. After PostProcComplete:
//   - Files must exist at completeDir/jobName/filename
//   - downloadDir/jobName/ must no longer exist
func TestNaming_FinalMoveToCompleteDir(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("final move test\n"), 64)
	files := []TestFile{
		{Name: "moved-file.bin", Payload: payload},
	}
	srv := newMockServer(t, files)

	a, downloadDir, completeDir := NewTestAppSeparateDirs(t, srv.Addr(), AppTestOpts{})
	rawNZB := BuildNZB(files)
	addNZBJob(t, a, rawNZB, "move-test")

	waitForPostProcWithTimeout(t, a, 30*time.Second)

	// File should be in completeDir.
	expectedPath := filepath.Join(completeDir, "move-test", "moved-file.bin")
	wantSHA := sha256.Sum256(payload)
	verifyFileAtPath(t, expectedPath, wantSHA[:])

	// Download dir should be cleaned up.
	downloadJobDir := filepath.Join(downloadDir, "move-test")
	verifyDirNotExists(t, downloadJobDir)
}
