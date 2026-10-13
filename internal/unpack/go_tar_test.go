package unpack

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// tarEntry describes one entry to write into a test tar archive.
type tarEntry struct {
	name     string
	typeflag byte // 0 defaults to tar.TypeReg
	mode     int64
	size     int64 // only used when content is nil
	content  []byte
	linkname string
	modTime  time.Time
	devmajor int64
	devminor int64
}

// buildTarBytes serializes entries into an in-memory tar archive and returns
// its raw bytes. This is the single place the byte-level construction logic
// lives: buildTar (below) writes these bytes to a file for tests that call
// GoTar against a path, and FuzzGoTar reuses it directly to seed the fuzz
// corpus with the same crafted-attack archives the named tests use.
//
// t is testing.TB (not *testing.T) so *testing.F -- which has no shared
// type with *testing.T beyond testing.TB -- can call this directly from
// FuzzGoTar's seed-corpus setup, before f.Fuzz has even been called.
func buildTarBytes(t testing.TB, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
			if typeflag == tar.TypeDir {
				mode = 0o755
			}
		}
		modTime := e.modTime
		if modTime.IsZero() {
			modTime = time.Now()
		}
		size := e.size
		if e.content != nil {
			size = int64(len(e.content))
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: typeflag,
			Mode:     mode,
			Size:     size,
			ModTime:  modTime,
			Linkname: e.linkname,
			Devmajor: e.devmajor,
			Devminor: e.devminor,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if e.content != nil {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("write content %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return buf.Bytes()
}

// buildTar writes entries into a new .tar file under dir and returns its
// path. Directory entries (typeflag == tar.TypeDir) get no content.
func buildTar(t *testing.T, dir, name string, entries []tarEntry) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buildTarBytes(t, entries), 0o644); err != nil { //nolint:gosec // test fixture path under t.TempDir()
		t.Fatalf("write tar: %v", err)
	}
	return path
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- (a) normal extraction ---

func TestGoTar_NormalExtraction(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "test.tar", []tarEntry{
		{name: "hello.txt", content: []byte("hello world")},
		{name: "sub/", typeflag: tar.TypeDir},
		{name: "sub/nested.txt", content: []byte("nested content")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	res, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	data, readErr := os.ReadFile(filepath.Join(outDir, "hello.txt"))
	if readErr != nil {
		t.Fatalf("read hello.txt: %v", readErr)
	}
	if string(data) != "hello world" {
		t.Errorf("hello.txt content = %q, want %q", data, "hello world")
	}

	nested, readErr := os.ReadFile(filepath.Join(outDir, "sub", "nested.txt"))
	if readErr != nil {
		t.Fatalf("read sub/nested.txt: %v", readErr)
	}
	if string(nested) != "nested content" {
		t.Errorf("sub/nested.txt content = %q, want %q", nested, "nested content")
	}

	if len(res.ExtractedFiles) != 2 {
		t.Errorf("ExtractedFiles = %v, want 2 entries", res.ExtractedFiles)
	}
}

// --- (b) path traversal rejected ---

func TestGoTar_PathTraversalRejected(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "traversal.tar", []tarEntry{
		{name: "../../../etc/passwd-pwned", content: []byte("evil")},
		{name: "safe.txt", content: []byte("safe content")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	// The safe entry must extract normally.
	if _, statErr := os.Stat(filepath.Join(outDir, "safe.txt")); statErr != nil {
		t.Errorf("safe.txt not extracted: %v", statErr)
	}

	// The traversal entry must not escape outDir. Check both possible
	// escape targets relative to outDir's parent.
	escaped := filepath.Join(filepath.Dir(outDir), "etc", "passwd-pwned")
	if _, statErr := os.Stat(escaped); statErr == nil {
		t.Errorf("path traversal entry escaped to %s", escaped)
	}
	// Also confirm it wasn't silently written inside outDir under a
	// mangled name that still contains "..".
	if _, statErr := os.Stat(filepath.Join(outDir, "..", "..", "..", "etc", "passwd-pwned")); statErr == nil {
		t.Errorf("path traversal entry escaped via relative outDir path")
	}
}

// --- (c) non-regular entry types skipped ---

func TestGoTar_NonRegularEntryTypesSkipped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		entryName string
		typeflag  byte
		linkname  string
		devmajor  int64
		devminor  int64
	}{
		{name: "symlink", entryName: "evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		{
			// Hardlinks reference an already-seen tar member by name (unlike
			// symlinks, which can point anywhere on the filesystem).
			name: "hardlink", entryName: "hard-link-to-safe", typeflag: tar.TypeLink, linkname: "safe.txt",
		},
		{name: "char device (TypeChar)", entryName: "evil-device", typeflag: tar.TypeChar, devmajor: 1, devminor: 5},
		{name: "block device (TypeBlock)", entryName: "evil-device", typeflag: tar.TypeBlock, devmajor: 1, devminor: 5},
		{name: "fifo", entryName: "evil-fifo", typeflag: tar.TypeFifo},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srcDir := t.TempDir()
			outDir := t.TempDir()

			tarPath := buildTar(t, srcDir, "nonreg.tar", []tarEntry{
				{name: "safe.txt", content: []byte("safe content")},
				{name: tc.entryName, typeflag: tc.typeflag, linkname: tc.linkname, devmajor: tc.devmajor, devminor: tc.devminor},
			})

			archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
			_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
			if err != nil {
				t.Fatalf("GoTar failed: err=%v", err)
			}

			if _, statErr := os.Lstat(filepath.Join(outDir, tc.entryName)); !os.IsNotExist(statErr) {
				t.Errorf("%s entry was created on disk (stat err: %v), should have been skipped", tc.name, statErr)
			}
			if _, statErr := os.Stat(filepath.Join(outDir, "safe.txt")); statErr != nil {
				t.Errorf("safe.txt not extracted: %v", statErr)
			}
		})
	}
}

// --- (c5) bare absolute path entry is defanged, not literally rejected ---

func TestGoTar_AbsolutePathEntryDefanged(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	// A bare absolute path (no ".." component at all) exercises a distinct
	// branch of SanitizeArchivePath (go_unrar.go) from
	// TestGoTar_PathTraversalRejected's relative "../../../etc/..." case.
	// Reading SanitizeArchivePath: it does path.Clean, then
	// strings.TrimPrefix(name, "/"), then rejects only if a ".."
	// component *remains* after cleaning. Clean("/etc/passwd") has no ".."
	// component, so TrimPrefix alone reduces it to "etc/passwd" -- the
	// entry is NOT rejected with an error; it is defanged (leading "/"
	// stripped) and extracted safely as a relative path contained inside
	// outDir. This is the same "strip the leading slash" convention GNU
	// tar itself uses by default. This test proves that behavior: no file
	// is ever written outside outDir, and the entry lands at outDir/etc/passwd.
	tarPath := buildTar(t, srcDir, "abspath.tar", []tarEntry{
		{name: "/etc/passwd", content: []byte("evil")},
		{name: "safe.txt", content: []byte("safe content")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	if _, statErr := os.Stat(filepath.Join(outDir, "safe.txt")); statErr != nil {
		t.Errorf("safe.txt not extracted: %v", statErr)
	}

	// The absolute-path entry must never land at the real, host-rooted
	// /etc/passwd.
	if _, statErr := os.Stat("/etc/passwd"); statErr == nil {
		data, readErr := os.ReadFile("/etc/passwd")
		if readErr == nil && string(data) == "evil" {
			t.Fatalf("absolute path entry escaped to real /etc/passwd")
		}
	}

	// It must be safely contained inside outDir, with the leading slash
	// stripped (outDir/etc/passwd), proving the escape was neutralized.
	contained, readErr := os.ReadFile(filepath.Join(outDir, "etc", "passwd"))
	if readErr != nil {
		t.Fatalf("expected absolute-path entry contained at outDir/etc/passwd: %v", readErr)
	}
	if string(contained) != "evil" {
		t.Errorf("outDir/etc/passwd content = %q, want %q", contained, "evil")
	}
}

// --- (c6) symlink entry whose *name* (not Linkname) contains ".." ---

func TestGoTar_SymlinkNameWithTraversal(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	// Proves the non-regular-type skip and path-sanitization checks
	// compose correctly: this symlink entry's *name* contains "..", not
	// its Linkname (FuzzGoTar's seed corpus covers a symlink with an
	// absolute Linkname but a clean name). Regardless of
	// which check -- type-based skip, or path sanitization -- would catch
	// this first, the outcome must be the same: nothing is created on
	// disk, so a future reordering of these checks can't accidentally let
	// a bad-path symlink through.
	tarPath := buildTar(t, srcDir, "symlink-traversal.tar", []tarEntry{
		{name: "../../evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		{name: "safe.txt", content: []byte("safe content")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	if _, statErr := os.Stat(filepath.Join(outDir, "safe.txt")); statErr != nil {
		t.Errorf("safe.txt not extracted: %v", statErr)
	}

	// Nothing should exist at the traversal target, whether escaped
	// outside outDir or written under a mangled name inside it.
	escaped := filepath.Join(filepath.Dir(outDir), "evil-link")
	if _, statErr := os.Lstat(escaped); statErr == nil {
		t.Errorf("symlink-with-traversal-name entry escaped to %s", escaped)
	}
	if _, statErr := os.Lstat(filepath.Join(outDir, "..", "evil-link")); statErr == nil {
		t.Errorf("symlink-with-traversal-name entry escaped via relative outDir path")
	}
	if _, statErr := os.Lstat(filepath.Join(outDir, "evil-link")); statErr == nil {
		t.Errorf("symlink-with-traversal-name entry was created inside outDir under a mangled name")
	}
}

// --- (d) setuid/setgid bits stripped ---

func TestGoTar_SetuidSetgidBitsStripped(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	// 0o4755 = setuid + rwxr-xr-x.
	tarPath := buildTar(t, srcDir, "setuid.tar", []tarEntry{
		{name: "suid-file", mode: 0o4755, content: []byte("payload")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	fi, statErr := os.Stat(filepath.Join(outDir, "suid-file"))
	if statErr != nil {
		t.Fatalf("stat suid-file: %v", statErr)
	}
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("setuid bit was not stripped: mode=%v", fi.Mode())
	}
	if fi.Mode().Perm()&0o4000 != 0 {
		t.Errorf("setuid bit present in raw perm bits: %o", fi.Mode().Perm())
	}
}

// --- (f) OneFolder flattening ---

func TestGoTar_OneFolderFlattens(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "flatten.tar", []tarEntry{
		{name: "a/b/c/deep.txt", content: []byte("deep content")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{OneFolder: true})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	if _, statErr := os.Stat(filepath.Join(outDir, "a")); statErr == nil {
		t.Errorf("subdirectory structure was preserved despite OneFolder")
	}
	data, readErr := os.ReadFile(filepath.Join(outDir, "deep.txt"))
	if readErr != nil {
		t.Fatalf("read flattened deep.txt: %v", readErr)
	}
	if string(data) != "deep content" {
		t.Errorf("deep.txt content = %q, want %q", data, "deep content")
	}
}

// --- (g) collision handling respects OverwriteFiles ---

func TestGoTar_OneFolderCollisionRenamesWhenNotOverwriting(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	// Two different subdirectories both contain a file named "same.txt" --
	// under OneFolder flattening these collide on the same basename.
	tarPath := buildTar(t, srcDir, "collide.tar", []tarEntry{
		{name: "dir1/same.txt", content: []byte("first")},
		{name: "dir2/same.txt", content: []byte("second")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{OneFolder: true, OverwriteFiles: false})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	first, readErr := os.ReadFile(filepath.Join(outDir, "same.txt"))
	if readErr != nil {
		t.Fatalf("read same.txt: %v", readErr)
	}
	if string(first) != "first" {
		t.Errorf("same.txt content = %q, want %q (first entry should not be overwritten)", first, "first")
	}

	renamed, readErr := os.ReadFile(filepath.Join(outDir, "same_1.txt"))
	if readErr != nil {
		t.Fatalf("expected collision-renamed same_1.txt, read failed: %v", readErr)
	}
	if string(renamed) != "second" {
		t.Errorf("same_1.txt content = %q, want %q", renamed, "second")
	}
}

func TestGoTar_NotAnArchive(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	notATar := filepath.Join(srcDir, "notatar.tar")
	if err := os.WriteFile(notATar, []byte("this is not a tar file, just plain garbage bytes"), 0o644); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}

	archive := Archive{Type: TarArchive, MainFile: notATar, Parts: []string{notATar}}
	res, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err == nil {
		t.Fatalf("expected error extracting garbage as tar, got success")
	}
	if res.Reason != FailCorrupt {
		t.Errorf("Reason = %v, want FailCorrupt", res.Reason)
	}
}

// --- OnLine callback coverage ---

// TestGoTar_OnLineCallback_NormalExtraction verifies opts.OnLine is invoked
// with an "Extracting" line for each regular-file entry on the success path.
func TestGoTar_OnLineCallback_NormalExtraction(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "online.tar", []tarEntry{
		{name: "hello.txt", content: []byte("hello world")},
	})

	var lines []string
	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{
		OnLine: func(line string) { lines = append(lines, line) },
	})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}
	if len(lines) == 0 {
		t.Fatal("OnLine was never called")
	}
	found := false
	for _, l := range lines {
		if strings.Contains(l, "hello.txt") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an OnLine callback mentioning hello.txt, got %v", lines)
	}
}

// TestGoTar_OnLineCallback_CorruptHeaderError verifies opts.OnLine is
// invoked with an "ERROR:" line when tr.Next() fails on a corrupt/non-tar
// stream (the nextErr != nil, non-EOF branch in goTarInternal).
func TestGoTar_OnLineCallback_CorruptHeaderError(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	notATar := filepath.Join(srcDir, "corrupt.tar")
	if err := os.WriteFile(notATar, []byte("garbage, not a tar header at all"), 0o644); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}

	var lines []string
	archive := Archive{Type: TarArchive, MainFile: notATar, Parts: []string{notATar}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{
		OnLine: func(line string) { lines = append(lines, line) },
	})
	if err == nil {
		t.Fatalf("expected error, got success")
	}
	if len(lines) == 0 {
		t.Fatal("OnLine was never called for corrupt header error")
	}
	if !strings.Contains(lines[0], "ERROR:") {
		t.Errorf("expected an ERROR line, got %v", lines)
	}
}

// TestGoTar_OnLineCallback_EntryError verifies opts.OnLine is invoked with
// an "ERROR:" line when a single entry fails extraction (the entryErr != nil
// branch in goTarInternal): a member under a path an earlier member wrote as
// a regular file cannot be created.
func TestGoTar_OnLineCallback_EntryError(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "entry-error.tar", []tarEntry{
		{name: "a", content: []byte("file")},
		{name: "a/b", content: []byte("under a file")},
	})

	var lines []string
	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{
		OnLine: func(line string) { lines = append(lines, line) },
	})
	if err == nil {
		t.Fatalf("expected an entry error, got success")
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "ERROR: a/b: ") }) {
		t.Errorf("expected an ERROR line for a/b, got %v", lines)
	}
}

// --- directory entry path sanitization ---

// TestGoTar_DirectoryEntryBadPathSkipped verifies a directory entry whose
// name fails SanitizeArchivePath (e.g. a ".." traversal component) is
// skipped rather than created, while later safe entries still extract.
func TestGoTar_DirectoryEntryBadPathSkipped(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "baddir.tar", []tarEntry{
		{name: "../../evil-dir/", typeflag: tar.TypeDir},
		{name: "safe.txt", content: []byte("safe content")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	escaped := filepath.Join(filepath.Dir(filepath.Dir(outDir)), "evil-dir")
	if _, statErr := os.Stat(escaped); statErr == nil {
		t.Errorf("traversal directory entry escaped to %s", escaped)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "safe.txt")); statErr != nil {
		t.Errorf("safe.txt not extracted: %v", statErr)
	}
}

// buildTarReaderEntry returns a one-entry tar reader positioned at a regular
// file named name holding size bytes.
func buildTarReaderEntry(t *testing.T, name string, size int) (*tar.Header, *tar.Reader) {
	t.Helper()
	content := bytes.Repeat([]byte{9}, size)
	hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(size), ModTime: time.Now()}
	return buildTarReaderEntryFromHeader(t, hdr, content)
}

// buildTarReaderEntryFromHeader writes a single arbitrary tar header (plus
// optional content) and returns the header and reader as tr.Next() sees them.
func buildTarReaderEntryFromHeader(t *testing.T, hdr *tar.Header, content []byte) (*tar.Header, *tar.Reader) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if len(content) > 0 {
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	tr := tar.NewReader(&buf)
	h, err := tr.Next()
	if err != nil {
		t.Fatalf("tr.Next: %v", err)
	}
	return h, tr
}

// --- zero rw-bits skip Chmod ---

// TestGoTar_ZeroPermissionBitsSkipsChmod verifies that an entry whose mode
// has no rw bits set after masking (hdr.Mode & 0o666 == 0, e.g. an
// execute-only 0o111 mode) skips the Chmod call entirely, leaving the file
// at the safe 0o600 default from OpenFile rather than attempting to chmod
// to 0.
func TestGoTar_ZeroPermissionBitsSkipsChmod(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "noperm.tar", []tarEntry{
		{name: "exec-only", mode: 0o111, content: []byte("payload")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	fi, statErr := os.Stat(filepath.Join(outDir, "exec-only"))
	if statErr != nil {
		t.Fatalf("stat exec-only: %v", statErr)
	}
	// The file must keep the safe rw-only default from OpenFile (0o600)
	// rather than having Chmod(mode=0) applied, which would strip all
	// access including the owner's.
	if fi.Mode().Perm()&0o600 != 0o600 {
		t.Errorf("expected owner rw bits preserved (Chmod skipped), got perm=%o", fi.Mode().Perm())
	}
}

// --- extractTarEntry OnLine callback coverage for non-regular / bad-path entries ---

// TestExtractTarEntry_OnLineCallbacks_NonRegularAndBadPath verifies
// extractTarEntry invokes opts.OnLine with a "Skipping" message both when a
// non-regular (symlink) entry is skipped and when a regular-file entry's
// path fails SanitizeArchivePath.
func TestExtractTarEntry_OnLineCallbacks_NonRegularAndBadPath(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "skips.tar", []tarEntry{
		{name: "evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		{name: "../../escape.txt", content: []byte("evil")},
		{name: "safe.txt", content: []byte("safe")},
	})

	var lines []string
	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{
		OnLine: func(line string) { lines = append(lines, line) },
	})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	var sawNonRegular, sawBadPath bool
	for _, l := range lines {
		if strings.Contains(l, "Skipping non-regular entry") {
			sawNonRegular = true
		}
		if strings.Contains(l, "Skipping bad path") {
			sawBadPath = true
		}
	}
	if !sawNonRegular {
		t.Errorf("expected an OnLine 'Skipping non-regular entry' message, got %v", lines)
	}
	if !sawBadPath {
		t.Errorf("expected an OnLine 'Skipping bad path' message, got %v", lines)
	}
}

// --- skip-existing-file OnLine callback coverage ---

// TestGoTar_SkipExistingFileOnLineCallback verifies that when
// opts.OverwriteFiles is false and the destination file already exists,
// extractTarFile logs and invokes opts.OnLine with a "Skipping existing"
// message rather than overwriting.
func TestGoTar_SkipExistingFileOnLineCallback(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "existing.tar", []tarEntry{
		{name: "payload.txt", content: []byte("new content")},
	})

	if err := os.WriteFile(filepath.Join(outDir, "payload.txt"), []byte("original content"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	var lines []string
	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{
		OverwriteFiles: false,
		OnLine:         func(line string) { lines = append(lines, line) },
	})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	data, readErr := os.ReadFile(filepath.Join(outDir, "payload.txt"))
	if readErr != nil {
		t.Fatalf("read payload.txt: %v", readErr)
	}
	if string(data) != "original content" {
		t.Errorf("payload.txt content = %q, want unchanged %q", data, "original content")
	}

	found := false
	for _, l := range lines {
		if strings.Contains(l, "Skipping existing") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an OnLine 'Skipping existing' message, got %v", lines)
	}
}

// --- GoTar panic recovery ---

// TestGoTar_PanicRecovery verifies that a panic inside goTarInternal (forced
// here by an OnLine that panics on its first call, the first entry's
// "Extracting" line) is recovered by cmdutil.SafeEngineRun, surfaces as an
// error mentioning "tar panic", sets res.Reason to FailCorrupt, and invokes
// opts.OnLine with the panic message -- mirroring
// TestGoSevenZip_PanicRecovery's equivalent proof for go_7z.
func TestGoTar_PanicRecovery(t *testing.T) {
	t.Parallel()

	tarPath := buildTar(t, t.TempDir(), "panic.tar", []tarEntry{
		{name: "a.txt", content: []byte("a")},
	})
	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}

	var onlineMsg string
	calls := 0
	res, err := GoTar(context.Background(), testLogger(), archive, t.TempDir(), Options{
		OnLine: func(msg string) {
			calls++
			if calls == 1 {
				panic("first OnLine call")
			}
			onlineMsg = msg
		},
	})
	if err == nil {
		t.Fatal("expected error from panic recovery, got nil")
	}
	if !strings.Contains(err.Error(), "tar panic") {
		t.Errorf("expected error to contain 'tar panic', got: %v", err)
	}
	if res.Reason != FailCorrupt {
		t.Errorf("expected res.Reason to be FailCorrupt, got: %v", res.Reason)
	}
	if !strings.Contains(onlineMsg, "ERROR: tar panic:") {
		t.Errorf("expected OnLine callback to receive 'ERROR: tar panic:', got: %q", onlineMsg)
	}
}

// --- classifyTarError direct coverage ---

func TestClassifyTarErrorDirect(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want FailReason
	}{
		{name: "nil error", err: nil, want: FailUnknown},
		{name: "tar header error", err: tar.ErrHeader, want: FailCorrupt},
		{name: "unclassified error", err: errors.New("some other error"), want: FailUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyTarError(tc.err); got != tc.want {
				t.Errorf("classifyTarError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --- IgnoreUnrarDates option ---

// TestGoTar_IgnoreUnrarDatesSkipsTimestampRestore verifies that when
// opts.IgnoreUnrarDates is true, the extracted file's mtime is not forced
// to match the archive entry's ModTime (it keeps the extraction-time mtime
// instead), proving the "!opts.IgnoreUnrarDates" guard actually gates the
// Chtimes call rather than always restoring timestamps.
func TestGoTar_IgnoreUnrarDatesSkipsTimestampRestore(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	archiveModTime := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	tarPath := buildTar(t, srcDir, "dates.tar", []tarEntry{
		{name: "dated.txt", content: []byte("payload"), modTime: archiveModTime},
	})

	before := time.Now().Add(-time.Second)

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{IgnoreUnrarDates: true})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	fi, statErr := os.Stat(filepath.Join(outDir, "dated.txt"))
	if statErr != nil {
		t.Fatalf("stat dated.txt: %v", statErr)
	}
	if fi.ModTime().Equal(archiveModTime) {
		t.Errorf("mtime was restored from archive (%v) despite IgnoreUnrarDates=true", archiveModTime)
	}
	if fi.ModTime().Before(before) {
		t.Errorf("mtime = %v, want recent extraction-time mtime (after %v)", fi.ModTime(), before)
	}
}

// --- structural edge cases (6c) ---

// TestGoTar_EmptyArchive verifies that a tar file containing zero entries --
// just the two 512-byte zero blocks tar.NewWriter's Close() always writes to
// mark end-of-archive -- extracts successfully with no files and no error.
func TestGoTar_EmptyArchive(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "empty.tar", nil)

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	res, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}
	if len(res.ExtractedFiles) != 0 {
		t.Errorf("ExtractedFiles = %v, want empty", res.ExtractedFiles)
	}
}

// TestGoTar_DirectoryOnlyArchive verifies an archive containing only
// tar.TypeDir entries (no regular files) creates the directories on disk.
// ExtractedFiles is the list publishStagedExtraction returns, which names
// only the files it renamed into outDir, never a directory it created. So a
// directory-only archive must report zero ExtractedFiles despite
// successfully creating directories.
func TestGoTar_DirectoryOnlyArchive(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "dirsonly.tar", []tarEntry{
		{name: "top/", typeflag: tar.TypeDir},
		{name: "top/nested/", typeflag: tar.TypeDir},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	res, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	for _, dir := range []string{"top", "top/nested"} {
		fi, statErr := os.Stat(filepath.Join(outDir, dir))
		if statErr != nil {
			t.Fatalf("stat %s: %v", dir, statErr)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}

	// No regular file was published, so ExtractedFiles must be empty.
	if len(res.ExtractedFiles) != 0 {
		t.Errorf("ExtractedFiles = %v, want empty (directories aren't tracked)", res.ExtractedFiles)
	}
}

// TestGoTar_PAXLongNameEntry verifies a filename long enough that
// archive/tar's writer must emit a PAX extended header (a >100-byte final
// path component can't fit in ustar's 100-byte Name field, nor be split into
// the 155-byte Name+Prefix scheme since the overflow is entirely within one
// component) still resolves to its full, correct nested path and passes
// through SanitizeArchivePath and extraction normally.
//
// Verified empirically against go1.27.2: tar.Writer
// auto-upgrades hdr.Format to PAX when needed -- there is no need to set
// hdr.Format = tar.FormatPAX explicitly, and tar.Reader.Next() returns the
// full resolved long name (not a truncated legacy-field value) with
// h.Format == tar.FormatPAX.
func TestGoTar_PAXLongNameEntry(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	// The final path component alone is 154 bytes, well over ustar's
	// 100-byte Name field limit, forcing archive/tar's writer to use a PAX
	// extended header regardless of the short "subdir/" prefix.
	longFileName := strings.Repeat("f", 150) + ".txt"
	longName := "subdir/" + longFileName
	if len(longName) <= 100 {
		t.Fatalf("test setup invalid: longName (%d bytes) must exceed the legacy 100-byte Name field", len(longName))
	}

	tarPath := buildTar(t, srcDir, "paxlongname.tar", []tarEntry{
		{name: longName, content: []byte("payload")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	res, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	data, readErr := os.ReadFile(filepath.Join(outDir, "subdir", longFileName))
	if readErr != nil {
		t.Fatalf("read long-named file at resolved nested path: %v", readErr)
	}
	if string(data) != "payload" {
		t.Errorf("content = %q, want %q", data, "payload")
	}
	if len(res.ExtractedFiles) != 1 {
		t.Errorf("ExtractedFiles = %v, want exactly 1 entry", res.ExtractedFiles)
	}
}

// TestGoTar_NonUTF8Filename pins down this codebase's existing behavior for
// an entry name containing invalid UTF-8 byte sequences: SanitizeArchivePath
// (go_unrar.go) only rejects null bytes and ".." components -- it performs
// no UTF-8 validation -- so an invalid-UTF-8 name that contains neither is
// neither rejected nor mangled. archive/tar itself round-trips the raw bytes
// unchanged (verified empirically: Writer/Reader preserve the exact byte
// sequence, auto-selecting PAX format for the header), and Linux filesystems
// accept any byte sequence in a filename except NUL and '/'. The net result,
// which this test documents rather than mandates, is that extraction
// succeeds and the file lands on disk under its literal (invalid-UTF-8) name.
func TestGoTar_NonUTF8Filename(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	// \xff\xfe is not valid UTF-8 in this position, but contains no NUL
	// byte and no ".." component, so SanitizeArchivePath's two checks
	// (null-byte rejection, ".." rejection) don't fire on it.
	badName := "bad-\xff\xfe-name.txt"

	tarPath := buildTar(t, srcDir, "nonutf8.tar", []tarEntry{
		{name: badName, content: []byte("payload")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	res, err := GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("GoTar failed: err=%v", err)
	}

	data, readErr := os.ReadFile(filepath.Join(outDir, badName))
	if readErr != nil {
		t.Fatalf("read invalid-UTF-8-named file: %v", readErr)
	}
	if string(data) != "payload" {
		t.Errorf("content = %q, want %q", data, "payload")
	}
	if len(res.ExtractedFiles) != 1 {
		t.Errorf("ExtractedFiles = %v, want exactly 1 entry", res.ExtractedFiles)
	}
}

// --- (6d) context cancellation mid-extraction ---

// TestGoTar_ContextCancellationMidExtraction verifies that goTarInternal's
// main loop checks ctx.Err() between tar entries (the check at the top of
// the for loop in go_tar.go, immediately before tr.Next()) and stops
// extraction promptly once the context is canceled, rather than draining
// the entire archive regardless of cancellation.
//
// The archive has 5 regular-file entries. opts.OnLine is invoked once per
// successfully-extracted entry (goTarInternal's "Extracting <name>" line,
// emitted right after extractTarEntry returns for that entry), so canceling
// the context from inside the first OnLine callback simulates "caller asked
// us to stop after entry 1 was already written" -- the earliest point a
// real caller (e.g. a job cancellation) could plausibly act. GoTar is fully
// synchronous with no internal goroutines, so no synchronization is needed
// around the shared `lines` slice or the cancel call.
func TestGoTar_ContextCancellationMidExtraction(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	const numEntries = 5
	entries := make([]tarEntry, 0, numEntries)
	for i := range numEntries {
		entries = append(entries, tarEntry{
			name:    fmt.Sprintf("file%d.txt", i),
			content: fmt.Appendf(nil, "contents of file %d", i),
		})
	}
	tarPath := buildTar(t, srcDir, "cancel.tar", entries)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var lines []string
	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := GoTar(ctx, testLogger(), archive, outDir, Options{
		OnLine: func(line string) {
			lines = append(lines, line)
			if len(lines) == 1 {
				cancel()
			}
		},
	})

	if err == nil {
		t.Fatal("expected an error from context cancellation, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected err to be/wrap context.Canceled, got: %v", err)
	}

	// The proof that extraction stopped early is the one "Extracting" line:
	// the top-of-loop ctx.Err() check must keep tr.Next() from reaching the
	// later entries. Nothing reaches outDir, since a cancelled extraction is
	// never published (extractInStage).
	if len(lines) != 1 {
		t.Errorf("OnLine lines = %q, want only the first entry's", lines)
	}
	if got := dirTree(t, outDir); len(got) != 0 {
		t.Errorf("outDir after a cancelled extraction = %q, want empty", got)
	}
}

// --- (6f) fuzzing ---

// FuzzGoTar fuzzes GoTar directly against raw tar bytes, modeled on
// internal/decoder/fuzz_test.go's FuzzDecodeArticle/FuzzDecodeUU harness
// style: seed the corpus with byte-serialized versions of every
// crafted-attack tar built in the named tests above (path traversal,
// symlink, hardlink, device, FIFO, oversized, absolute-path, PAX-long-name,
// non-UTF8 filename), then let go test -fuzz mutate them.
//
// The only invariant asserted is "does not panic": TestGoTar_PanicRecovery
// above already proves cmdutil.SafeEngineRun catches panics from inside
// goTarInternal and turns them into a returned error, so a panic that
// escapes this fuzz target (crashing the test binary instead of being
// returned as an error) would mean a panic path *outside* that recovery
// wrapper -- a genuine bug, not an expected fuzz failure mode.
func FuzzGoTar(f *testing.F) {
	seed := func(entries []tarEntry) {
		f.Add(buildTarBytes(f, entries))
	}

	// Path traversal (relative "../..").
	seed([]tarEntry{
		{name: "../../../etc/passwd-pwned", content: []byte("evil")},
		{name: "safe.txt", content: []byte("safe content")},
	})
	// Bare absolute path (no ".." component).
	seed([]tarEntry{
		{name: "/etc/passwd", content: []byte("evil")},
		{name: "safe.txt", content: []byte("safe content")},
	})
	// Symlink with an absolute Linkname.
	seed([]tarEntry{
		{name: "evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		{name: "safe.txt", content: []byte("safe content")},
	})
	// Symlink whose own name (not Linkname) contains "..".
	seed([]tarEntry{
		{name: "../../evil-link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		{name: "safe.txt", content: []byte("safe content")},
	})
	// Hardlink referencing an already-seen tar member.
	seed([]tarEntry{
		{name: "safe.txt", content: []byte("safe content")},
		{name: "hard-link-to-safe", typeflag: tar.TypeLink, linkname: "safe.txt"},
	})
	// Char and block device entries.
	seed([]tarEntry{
		{name: "evil-char-device", typeflag: tar.TypeChar, devmajor: 1, devminor: 5},
		{name: "safe.txt", content: []byte("safe content")},
	})
	seed([]tarEntry{
		{name: "evil-block-device", typeflag: tar.TypeBlock, devmajor: 1, devminor: 5},
		{name: "safe.txt", content: []byte("safe content")},
	})
	// FIFO entry.
	seed([]tarEntry{
		{name: "evil-fifo", typeflag: tar.TypeFifo},
		{name: "safe.txt", content: []byte("safe content")},
	})
	// A large zero-filled entry.
	seed([]tarEntry{
		{name: "huge-file", content: bytes.Repeat([]byte{0}, 64*1024)},
	})
	// PAX extended header via a name exceeding ustar's 100-byte limit.
	seed([]tarEntry{
		{name: "subdir/" + strings.Repeat("f", 150) + ".txt", content: []byte("payload")},
	})
	// Invalid-UTF-8 filename.
	seed([]tarEntry{
		{name: "bad-\xff\xfename.txt", content: []byte("payload")},
	})
	// setuid/setgid bits on a regular file.
	seed([]tarEntry{
		{name: "suid-file", mode: 0o4755, content: []byte("payload")},
	})
	// Empty archive (no entries at all -- just buildTarBytes's terminating
	// zero blocks).
	seed(nil)

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		tarPath := filepath.Join(dir, "fuzz.tar")
		if err := os.WriteFile(tarPath, data, 0o644); err != nil { //nolint:gosec // fuzz fixture path under t.TempDir()
			t.Fatalf("write fuzz tar fixture: %v", err)
		}
		outDir := filepath.Join(dir, "out")
		if err := os.Mkdir(outDir, 0o755); err != nil {
			t.Fatalf("mkdir outDir: %v", err)
		}

		archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
		// Only "does not panic" is asserted: errors (corrupt archive,
		// rejected paths) are all expected and valid
		// outcomes for arbitrary mutated input.
		_, _ = GoTar(context.Background(), testLogger(), archive, outDir, Options{})
	})
}

// TestGoTarInternal_Direct calls goTarInternal directly rather than through
// the exported GoTar (which only adds panic recovery via
// cmdutil.SafeEngineRun) — proving the underlying implementation the tests
// above exercise indirectly actually behaves as documented when called on
// its own.
func TestGoTarInternal_Direct(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "test.tar", []tarEntry{
		{name: "direct.txt", content: []byte("direct call content")},
	})

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	_, err := goTarInternal(context.Background(), testLogger(), archive, outDir, Options{})
	if err != nil {
		t.Fatalf("goTarInternal failed: err=%v", err)
	}

	data, readErr := os.ReadFile(filepath.Join(outDir, "direct.txt"))
	if readErr != nil {
		t.Fatalf("read direct.txt: %v", readErr)
	}
	if string(data) != "direct call content" {
		t.Errorf("direct.txt content = %q, want %q", data, "direct call content")
	}
}

// TestExtractTarEntry_Direct covers extractTarEntry's own dispatch logic
// (type filtering: regular file, directory, symlink) directly.
func TestExtractTarEntry_Direct(t *testing.T) {
	t.Parallel()

	t.Run("regular file is extracted and returns true", func(t *testing.T) {
		t.Parallel()
		outDir, root := openTestRoot(t)

		hdr, tr := buildTarReaderEntry(t, "entry.txt", 11)
		extracted, err := extractTarEntry(t.Context(), root, outDir, tr, hdr, Options{}, testLogger())
		if err != nil {
			t.Fatalf("extractTarEntry: %v", err)
		}
		if !extracted {
			t.Error("extracted = false, want true for a regular file entry")
		}
		if _, statErr := os.Stat(filepath.Join(outDir, "entry.txt")); statErr != nil {
			t.Errorf("expected entry.txt to exist: %v", statErr)
		}
	})

	t.Run("directory entry creates the dir and returns false", func(t *testing.T) {
		t.Parallel()
		outDir, root := openTestRoot(t)

		hdr := &tar.Header{Name: "subdir/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Now()}
		gotHdr, tr := buildTarReaderEntryFromHeader(t, hdr, nil)

		extracted, err := extractTarEntry(t.Context(), root, outDir, tr, gotHdr, Options{}, testLogger())
		if err != nil {
			t.Fatalf("extractTarEntry: %v", err)
		}
		if extracted {
			t.Error("extracted = true, want false for a directory entry")
		}
		info, statErr := os.Stat(filepath.Join(outDir, "subdir"))
		if statErr != nil || !info.IsDir() {
			t.Errorf("expected subdir/ to exist as a directory: %v", statErr)
		}
	})

	t.Run("symlink entry is skipped and returns false", func(t *testing.T) {
		t.Parallel()
		outDir, root := openTestRoot(t)

		hdr := &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777, ModTime: time.Now()}
		gotHdr, tr := buildTarReaderEntryFromHeader(t, hdr, nil)

		extracted, err := extractTarEntry(t.Context(), root, outDir, tr, gotHdr, Options{}, testLogger())
		if err != nil {
			t.Fatalf("extractTarEntry: %v", err)
		}
		if extracted {
			t.Error("extracted = true, want false for a symlink entry")
		}
		if _, statErr := os.Lstat(filepath.Join(outDir, "link")); !os.IsNotExist(statErr) {
			t.Errorf("expected no symlink to be created, stat err = %v", statErr)
		}
	})
}

// openTestRoot creates a fresh temp directory and an *os.Root over it,
// registering cleanup of the root handle on test completion.
func openTestRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatalf("os.OpenRoot: %v", err)
	}
	t.Cleanup(func() {
		_ = root.Close()
	})
	return outDir, root
}

// TestGoTar_ADanglingSymlinkOccupiesTheName pins writeEntrySafely's Lstat.
//
// OverwriteFiles=false promises an existing file is skipped, and Stat answers
// about a link's TARGET rather than the link, so a DANGLING symlink read as no
// file at all and the entry replaced it. The link is reachable: the external
// unrar/7z paths never go through writeEntrySafely and can extract a symlink
// into the same output directory a later internal extraction writes to. Its
// non-test callers are the three native engines — go_tar.go:231,
// go_sevenzip.go:241 and go_unrar.go:210 — and neither unrar.go nor
// sevenzip.go, which drive the external tools, is among them.
//
// This is not a containment hole — the write goes to a temp file and
// root.Rename, which does not follow a final symlink — so what is at stake is
// the flag's stated contract, not an escape.
func TestGoTar_ADanglingSymlinkOccupiesTheName(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	outDir := t.TempDir()

	tarPath := buildTar(t, srcDir, "skip.tar", []tarEntry{
		{name: "payload.bin", content: []byte("from the archive")},
	})

	// Relative and dangling: nothing at the target, so Stat reports the name
	// free while Lstat reports the link itself.
	if err := os.Symlink("missing.bin", filepath.Join(outDir, "payload.bin")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	archive := Archive{Type: TarArchive, MainFile: tarPath, Parts: []string{tarPath}}
	if _, err := GoTar(context.Background(), testLogger(), archive, outDir,
		Options{OverwriteFiles: false}); err != nil {
		t.Fatalf("GoTar: %v", err)
	}

	info, err := os.Lstat(filepath.Join(outDir, "payload.bin"))
	if err != nil {
		t.Fatalf("Lstat after extraction: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the archive entry replaced an existing symlink though OverwriteFiles is false; " +
			"Stat answers about the link's target, so a dangling link read as an unoccupied name")
	}
}
