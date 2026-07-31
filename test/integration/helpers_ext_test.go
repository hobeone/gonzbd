//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/app"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/queue"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

// ---------------------------------------------------------------------------
// Tool availability helpers
// ---------------------------------------------------------------------------

// requireTool skips the test if the named binary is not in PATH.
func requireTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed; skipping", name)
	}
	return path
}

// ---------------------------------------------------------------------------
// Strict path verification (unlike findFile, these catch directory bugs)
// ---------------------------------------------------------------------------

// verifyFileAtPath asserts a file exists at the exact path and optionally
// verifies its SHA-256 digest. Pass nil wantSHA to skip content check.
func verifyFileAtPath(t *testing.T, path string, wantSHA []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		// List parent dir contents to aid debugging.
		parent := filepath.Dir(path)
		entries, _ := os.ReadDir(parent)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("file %q does not exist; parent dir %q contains: %v", path, parent, names)
	}
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if info.IsDir() {
		t.Fatalf("expected file at %q but found directory", path)
	}
	if wantSHA == nil {
		return
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: test code
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], wantSHA) {
		t.Errorf("SHA-256 mismatch for %s:\n  got  %x\n  want %x", filepath.Base(path), got[:], wantSHA)
	}
}

// verifyDirExists asserts the path exists and is a directory.
func verifyDirExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		t.Fatalf("directory %q does not exist", path)
	}
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected directory at %q but found file", path)
	}
}

// verifyDirNotExists asserts the path does not exist.
func verifyDirNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		entries, _ := os.ReadDir(path)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory %q should not exist but does; contents: %v", path, names)
	}
}

// ---------------------------------------------------------------------------
// Subject format variants for NZB builder
// ---------------------------------------------------------------------------

// SubjectFormat controls how the NZB subject line is formatted.
type SubjectFormat int

const (
	// SubjectQuoted is the standard format: [1/N] - "filename" yEnc (1/N) SIZE
	SubjectQuoted SubjectFormat = iota
	// SubjectPRiVATE uses: [PRiVATE]-[WtFnZb]-[filename]-[N/M] - "" yEnc SIZE (1/N)
	SubjectPRiVATE
	// SubjectBare uses just: filename yEnc (1/N)
	SubjectBare
)

// BuildNZBCustomSubject produces NZB XML with the given subject format.
func BuildNZBCustomSubject(files []TestFile, format SubjectFormat) []byte {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	sb.WriteString(`<!DOCTYPE nzb PUBLIC "-//newzBin//DTD NZB 1.1//EN" "http://www.newzbin.com/DTD/nzb/nzb-1.1.dtd">` + "\n")
	sb.WriteString(`<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">` + "\n")

	now := time.Now().Unix()

	for fileNum, f := range files {
		partSize := f.PartSize
		if partSize <= 0 {
			partSize = len(f.Payload)
			if partSize == 0 {
				partSize = 1
			}
		}
		parts := SplitIntoParts(f.Payload, partSize)
		totalParts := len(parts)

		totalDeclared := 0
		for _, p := range parts {
			totalDeclared += len(p)
		}

		var subject string
		switch format {
		case SubjectPRiVATE:
			subject = fmt.Sprintf("[PRiVATE]-[WtFnZb]-[%s]-[%d/%d] - &quot;&quot; yEnc  %d (1/%d)",
				f.Name, fileNum+1, len(files), totalDeclared, totalParts)
		case SubjectBare:
			subject = fmt.Sprintf("%s yEnc (1/%d)", f.Name, totalParts)
		default: // SubjectQuoted
			subject = fmt.Sprintf("[1/%d] - &quot;%s&quot; yEnc (1/%d) %d",
				totalParts, f.Name, totalParts, totalDeclared)
		}

		fmt.Fprintf(&sb, "  <file poster=\"poster@integration.test\" date=\"%d\" subject=\"%s\">\n", now, subject)
		sb.WriteString("    <groups><group>alt.test</group></groups>\n")
		sb.WriteString("    <segments>\n")

		var offset int64
		for i, part := range parts {
			partNum := i + 1
			mid := articleID(f.Name, partNum)
			fmt.Fprintf(&sb, "      <segment bytes=\"%d\" number=\"%d\">%s</segment>\n",
				len(part), partNum, mid)
			offset += int64(len(part))
		}

		sb.WriteString("    </segments>\n")
		sb.WriteString("  </file>\n")
	}

	sb.WriteString("</nzb>\n")
	return []byte(sb.String())
}

// ---------------------------------------------------------------------------
// App construction with separate download/complete dirs
// ---------------------------------------------------------------------------

// AppTestOpts configures the test app for post-processing tests.
type AppTestOpts struct {
	EnableUnrar  bool
	Enable7zip   bool
	Par2Command  string
	FolderRename bool
	FlatUnpack   bool
	Nice         string
}

// NewTestAppSeparateDirs builds and starts an Application with distinct
// downloadDir and completeDir. Returns both paths for verification.
func NewTestAppSeparateDirs(t *testing.T, mockAddr string, opts AppTestOpts) (a *app.Application, downloadDir, completeDir string) {
	t.Helper()

	downloadDir = filepath.Join(t.TempDir(), "incomplete")
	completeDir = filepath.Join(t.TempDir(), "complete")
	adminDir := filepath.Join(t.TempDir(), "admin")

	for _, d := range []string{downloadDir, completeDir, adminDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	cfg := buildAppConfig(mockAddr, downloadDir)
	cfg.With(func(c *config.Config) {
		c.General.CompleteDir = completeDir
		c.General.AdminDir = adminDir
		c.PostProc.EnableUnrar = opts.EnableUnrar
		c.PostProc.Enable7zip = opts.Enable7zip
		c.PostProc.FolderRename = opts.FolderRename
		c.PostProc.FlatUnpack = opts.FlatUnpack
		c.PostProc.Nice = opts.Nice
		if opts.Par2Command != "" {
			c.PostProc.Par2Command = opts.Par2Command
		}
	})

	db, err := history.Open(t.Context(), filepath.Join(adminDir, "history.db"))
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	repo := history.NewRepository(db)

	a, err = app.New(cfg, repo)
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
		if err := a.Shutdown(); err != nil {
			t.Logf("app.Shutdown: %v", err)
		}
		db.Close()
	})

	return a, downloadDir, completeDir
}

// ---------------------------------------------------------------------------
// Fixture creation helpers
// ---------------------------------------------------------------------------



// createPar2Fixture creates a par2 recovery set for the named files in dir.
// Returns the path to the main .par2 file. Skips if par2 is not available.
func createPar2Fixture(t *testing.T, dir string, filenames ...string) string {
	t.Helper()
	requireTool(t, "par2")

	parFile := filepath.Join(dir, "recovery.par2")
	args := append([]string{"create", "-r5", "-n1", parFile}, filenames...)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	//nolint:gosec // G204: test code
	cmd := exec.CommandContext(ctx, "par2", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("par2 create: %v\noutput: %s", err, out)
	}
	return parFile
}

// create7zFixture creates a 7z archive in dir. Returns the archive path.
// Skips if 7z is not available.
func create7zFixture(t *testing.T, dir, archiveName string, files map[string][]byte) string {
	t.Helper()
	bin := requireTool(t, "7z")

	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	archivePath := filepath.Join(dir, archiveName)
	args := []string{"a", archivePath}
	for name := range files {
		args = append(args, filepath.Join(dir, name))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	//nolint:gosec // G204: test code
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7z create %s: %v\noutput: %s", archiveName, err, out)
	}

	// Delete source files.
	for name := range files {
		os.Remove(filepath.Join(dir, name))
	}

	return archivePath
}

// fixtureToTestFiles reads all files from dir and wraps them as TestFile
// entries suitable for RegisterArticles and BuildNZB. Each file becomes a
// multi-part article when larger than partSize bytes.
func fixtureToTestFiles(t *testing.T, dir string, partSize int) []TestFile {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var files []TestFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: test code
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		files = append(files, TestFile{
			Name:     e.Name(),
			Payload:  data,
			PartSize: partSize,
		})
	}
	if len(files) == 0 {
		t.Fatalf("no files found in fixture dir %s", dir)
	}
	return files
}

// fixtureToTestFilesRecursive is like fixtureToTestFiles but walks
// subdirectories recursively. File names include their relative path
// (e.g. "Release-GROUP/movie.rar"), which causes the assembler to
// create matching subdirectories inside the job's download directory.
// This is essential for testing the common Usenet pattern where NZBs
// organize files into release-name subdirectories.
func fixtureToTestFilesRecursive(t *testing.T, dir string, partSize int) []TestFile {
	t.Helper()
	var files []TestFile
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(path) //nolint:gosec // G304: test code
		if readErr != nil {
			return readErr
		}
		files = append(files, TestFile{
			Name:     rel,
			Payload:  data,
			PartSize: partSize,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no files found in fixture dir %s", dir)
	}
	return files
}

// waitForPostProcWithTimeout waits for PostProcComplete or times out.
func waitForPostProcWithTimeout(t *testing.T, a *app.Application, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	select {
	case <-a.PostProcComplete():
		// success
	case <-ctx.Done():
		t.Fatalf("timeout (%v) waiting for PostProcComplete", timeout)
	}
}

// addNZBJobPP is like addNZBJob but sets the post-processing level.
// PP=3 enables repair + unpack + cleanup; PP=0 is download-only.
func addNZBJobPP(t *testing.T, a *app.Application, rawNZB []byte, name string, pp int) *queue.Job {
	t.Helper()
	parsed, err := nzb.Parse(bytes.NewReader(rawNZB))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	job, err := queue.NewJob(parsed, queue.AddOptions{
		Filename: name + ".nzb",
		Name:     name,
		PP:       pp,
	}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("queue.NewJob: %v", err)
	}
	if err := a.AddJob(t.Context(), job, rawNZB, false); err != nil {
		t.Fatalf("app.AddJob: %v", err)
	}
	return job
}

// newMockServerFromFixtures registers fixture TestFiles with a mock NNTP
// server and starts it. Registers cleanup with t.Cleanup.
func newMockServerFromFixtures(t *testing.T, files []TestFile) *mocknntp.Server {
	t.Helper()
	srv := mocknntp.NewServer(mocknntp.Config{})
	RegisterArticles(srv, files)
	if err := srv.Start(); err != nil {
		t.Fatalf("mock server start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// copyFixture copies a pre-generated fixture from test/fixtures/integration/ to destDir.
func copyFixture(t *testing.T, srcName, destDir, destName string) string {
	t.Helper()
	src := filepath.Join("..", "..", "test", "fixtures", "integration", srcName)
	absSrc, err := filepath.Abs(src)
	if err != nil {
		t.Fatal(fmt.Errorf("absolute path of %s: %w", src, err))
	}
	content, err := os.ReadFile(absSrc)
	if err != nil {
		t.Fatal(fmt.Errorf("read fixture %s: %w", absSrc, err))
	}
	dest := filepath.Join(destDir, destName)
	if err := os.WriteFile(dest, content, 0o600); err != nil {
		t.Fatal(fmt.Errorf("write file %s: %w", dest, err))
	}
	return dest
}
