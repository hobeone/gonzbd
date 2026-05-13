package unpack_test

import (
	"bytes"
	"cmp"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// ---- helpers -----------------------------------------------------------------

// touch creates an empty regular file at path, making parent dirs as needed.
func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// write creates a file at path with the given content.
func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---- Classify tests ----------------------------------------------------------

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want unpack.ArchiveType
	}{
		{"movie.rar", unpack.RarArchive},
		{"movie.RAR", unpack.RarArchive},
		{"movie.part01.rar", unpack.RarArchive},
		{"movie.part1.rar", unpack.RarArchive},
		{"movie.r00", unpack.RarArchive},
		{"movie.r99", unpack.RarArchive},
		{"archive.7z", unpack.SevenZipArchive},
		{"archive.7Z", unpack.SevenZipArchive},
		{"archive.7z.001", unpack.SevenZipArchive},
		{"archive.7z.002", unpack.SevenZipArchive},
		{"data.001", unpack.SplitArchive},
		{"data.002", unpack.SplitArchive},
		// P21: .ts.NNN files are generic splits — joined by FileJoin
		{"show.ts.001", unpack.SplitArchive},
		{"show.ts.002", unpack.SplitArchive},
		{"readme.txt", unpack.UnknownArchive},
		{"movie.nfo", unpack.UnknownArchive},
		{"noext", unpack.UnknownArchive},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			got := unpack.Classify(tc.path)
			if got != tc.want {
				t.Errorf("Classify(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// ---- Scan tests --------------------------------------------------------------

func TestScan(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		layout  []string
		wantLen int
		// wantNames is a sorted list of archive set names expected.
		wantNames []string
		// wantTypes maps set name → expected ArchiveType.
		wantTypes map[string]unpack.ArchiveType
	}{
		{
			name: "new-style multipart RAR",
			layout: []string{
				"movie.part01.rar",
				"movie.part02.rar",
				"movie.part03.rar",
				"readme.nfo",
			},
			wantLen:   1,
			wantNames: []string{"movie"},
			wantTypes: map[string]unpack.ArchiveType{"movie": unpack.RarArchive},
		},
		{
			name: "legacy RAR set",
			layout: []string{
				"show.rar",
				"show.r00",
				"show.r01",
			},
			wantLen:   1,
			wantNames: []string{"show"},
			wantTypes: map[string]unpack.ArchiveType{"show": unpack.RarArchive},
		},
		{
			name: "single 7z archive",
			layout: []string{
				"backup.7z",
			},
			wantLen:   1,
			wantNames: []string{"backup"},
			wantTypes: map[string]unpack.ArchiveType{"backup": unpack.SevenZipArchive},
		},
		{
			name: "split 7z volumes",
			layout: []string{
				"big.7z.001",
				"big.7z.002",
				"big.7z.003",
			},
			wantLen:   1,
			wantNames: []string{"big"},
			wantTypes: map[string]unpack.ArchiveType{"big": unpack.SevenZipArchive},
		},
		{
			name: "generic split files",
			layout: []string{
				"data.001",
				"data.002",
				"data.003",
			},
			wantLen:   1,
			wantNames: []string{"data"},
			wantTypes: map[string]unpack.ArchiveType{"data": unpack.SplitArchive},
		},
		{
			name: "mixed archive types",
			layout: []string{
				"alpha.part01.rar",
				"alpha.part02.rar",
				"beta.7z",
				"gamma.001",
				"gamma.002",
				"ignore.txt",
			},
			wantLen:   3,
			wantNames: []string{"alpha", "beta", "gamma"},
			wantTypes: map[string]unpack.ArchiveType{
				"alpha": unpack.RarArchive,
				"beta":  unpack.SevenZipArchive,
				"gamma": unpack.SplitArchive,
			},
		},
		{
			name:    "empty directory",
			layout:  []string{},
			wantLen: 0,
		},
		{
			name: "non-archive files only",
			layout: []string{
				"readme.txt",
				"image.jpg",
			},
			wantLen: 0,
		},
		{
			// P21: .ts.NNN files are generic splits with name "show.ts"
			name: "ts split files",
			layout: []string{
				"show.ts.001",
				"show.ts.002",
				"show.ts.003",
			},
			wantLen:   1,
			wantNames: []string{"show.ts"},
			wantTypes: map[string]unpack.ArchiveType{"show.ts": unpack.SplitArchive},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for _, f := range tc.layout {
				touch(t, filepath.Join(dir, f))
			}

			archives, err := unpack.Scan(dir)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}

			if len(archives) != tc.wantLen {
				t.Fatalf("Scan returned %d archives, want %d: %+v", len(archives), tc.wantLen, archives)
			}

			slices.SortFunc(archives, func(a, b unpack.Archive) int { return cmp.Compare(a.Name, b.Name) })

			for i, name := range tc.wantNames {
				if archives[i].Name != name {
					t.Errorf("archives[%d].Name = %q, want %q", i, archives[i].Name, name)
				}
			}
			for _, a := range archives {
				wantType, ok := tc.wantTypes[a.Name]
				if !ok {
					continue
				}
				if a.Type != wantType {
					t.Errorf("archive %q: Type = %v, want %v", a.Name, a.Type, wantType)
				}
			}
		})
	}
}

// TestScanNewStyleRARMainFile verifies that the first part (part01) is selected
// as the MainFile when scanning a new-style multi-part RAR set.
func TestScanNewStyleRARMainFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, f := range []string{"movie.part01.rar", "movie.part02.rar", "movie.part03.rar"} {
		touch(t, filepath.Join(dir, f))
	}

	archives, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("want 1 archive, got %d", len(archives))
	}
	a := archives[0]
	if got := filepath.Base(a.MainFile); got != "movie.part01.rar" {
		t.Errorf("MainFile = %q, want movie.part01.rar", got)
	}
	if len(a.Parts) != 3 {
		t.Errorf("len(Parts) = %d, want 3", len(a.Parts))
	}
}

// ---- FileJoin tests ----------------------------------------------------------

func TestFileJoin(t *testing.T) {
	t.Parallel()

	part1 := []byte("Hello, ")
	part2 := []byte("World!")
	want := append(part1, part2...) //nolint:gocritic // intentional append into new slice

	dir := t.TempDir()
	outDir := t.TempDir()

	write(t, filepath.Join(dir, "data.001"), part1)
	write(t, filepath.Join(dir, "data.002"), part2)

	archive := unpack.Archive{
		Type:     unpack.SplitArchive,
		Name:     "data",
		MainFile: filepath.Join(dir, "data.001"),
		Parts: []string{
			filepath.Join(dir, "data.001"),
			filepath.Join(dir, "data.002"),
		},
	}

	res, err := unpack.FileJoin(t.Context(), slog.Default(), archive, outDir, unpack.Options{})
	if err != nil {
		t.Fatalf("FileJoin: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("Result.Err: %v", res.Err)
	}

	outPath := filepath.Join(outDir, "data")
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("joined output = %q, want %q", got, want)
	}
}

// P21: FileJoin with .ts.NNN files produces show.ts as output.
func TestFileJoin_TSFiles(t *testing.T) {
	t.Parallel()

	part1 := []byte("TS segment 1 ")
	part2 := []byte("TS segment 2 ")
	part3 := []byte("TS segment 3")
	want := append(append(part1, part2...), part3...)

	dir := t.TempDir()
	outDir := t.TempDir()

	write(t, filepath.Join(dir, "show.ts.001"), part1)
	write(t, filepath.Join(dir, "show.ts.002"), part2)
	write(t, filepath.Join(dir, "show.ts.003"), part3)

	archive := unpack.Archive{
		Type:     unpack.SplitArchive,
		Name:     "show.ts",
		MainFile: filepath.Join(dir, "show.ts.001"),
		Parts: []string{
			filepath.Join(dir, "show.ts.001"),
			filepath.Join(dir, "show.ts.002"),
			filepath.Join(dir, "show.ts.003"),
		},
	}

	res, err := unpack.FileJoin(t.Context(), slog.Default(), archive, outDir, unpack.Options{})
	if err != nil {
		t.Fatalf("FileJoin: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("Result.Err: %v", res.Err)
	}

	outPath := filepath.Join(outDir, "show.ts")
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("joined output = %q, want %q", got, want)
	}
}

func TestFileJoin_WrongType(t *testing.T) {
	t.Parallel()

	archive := unpack.Archive{Type: unpack.RarArchive, Name: "x"}
	_, err := unpack.FileJoin(t.Context(), slog.Default(), archive, t.TempDir(), unpack.Options{})
	if err == nil {
		t.Fatal("expected error for non-SplitArchive, got nil")
	}
}

func TestFileJoin_ExistingOutputNoOp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outDir := t.TempDir()

	write(t, filepath.Join(dir, "x.001"), []byte("a"))
	write(t, filepath.Join(dir, "x.002"), []byte("b"))
	// Pre-create the output file.
	write(t, filepath.Join(outDir, "x"), []byte("existing"))

	archive := unpack.Archive{
		Type:     unpack.SplitArchive,
		Name:     "x",
		MainFile: filepath.Join(dir, "x.001"),
		Parts:    []string{filepath.Join(dir, "x.001"), filepath.Join(dir, "x.002")},
	}
	res, err := unpack.FileJoin(t.Context(), slog.Default(), archive, outDir, unpack.Options{})
	if err != nil {
		t.Fatalf("expected no-op success when output exists, got error: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("expected no-op success, got Result.Err: %v", res.Err)
	}
	// Existing content should be preserved (not overwritten).
	got, _ := os.ReadFile(filepath.Join(outDir, "x"))
	if string(got) != "existing" {
		t.Errorf("existing file was modified: got %q, want %q", got, "existing")
	}
}

func TestFileJoin_ContextCancelled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outDir := t.TempDir()

	write(t, filepath.Join(dir, "big.001"), bytes.Repeat([]byte("x"), 1024))
	write(t, filepath.Join(dir, "big.002"), bytes.Repeat([]byte("y"), 1024))

	archive := unpack.Archive{
		Type:     unpack.SplitArchive,
		Name:     "big",
		MainFile: filepath.Join(dir, "big.001"),
		Parts:    []string{filepath.Join(dir, "big.001"), filepath.Join(dir, "big.002")},
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	_, err := unpack.FileJoin(ctx, slog.Default(), archive, outDir, unpack.Options{})
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
	// Output file should be cleaned up.
	if _, statErr := os.Stat(filepath.Join(outDir, "big")); statErr == nil {
		t.Error("partial output file was not cleaned up after context cancellation")
	}
}

// ---- Integration tests (require unrar / 7zz binaries) ----------------------

func TestUnRAR_Integration(t *testing.T) {
	if _, err := exec.LookPath("unrar"); err != nil {
		t.Skip("unrar binary not found in PATH; skipping integration test")
	}

	// We ship a small pre-generated test.rar in testdata/.
	// If the file doesn't exist, skip rather than fail.
	rarPath := "testdata/test.rar"
	if _, err := os.Stat(rarPath); err != nil {
		t.Skipf("testdata/test.rar not present (%v); skipping unrar integration test", err)
	}

	outDir := t.TempDir()
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: rarPath,
		Parts:    []string{rarPath},
	}

	res, err := unpack.UnRAR(t.Context(), slog.Default(), archive, outDir, unpack.Options{})
	if err != nil {
		t.Fatalf("UnRAR error: %v\nOutput:\n%s", err, res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("UnRAR exit code %d\nOutput:\n%s", res.ExitCode, res.Output)
	}
}

func TestSevenZip_Integration(t *testing.T) {
	bin, err := exec.LookPath("7zz")
	if err != nil {
		bin, err = exec.LookPath("7z")
	}
	if err != nil || bin == "" {
		t.Skip("7zz/7z binary not found in PATH; skipping integration test")
	}

	// We ship a small pre-generated test.7z in testdata/.
	szPath := "testdata/test.7z"
	if _, err := os.Stat(szPath); err != nil {
		t.Skipf("testdata/test.7z not present (%v); skipping 7zip integration test", err)
	}

	outDir := t.TempDir()
	archive := unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: szPath,
		Parts:    []string{szPath},
	}

	res, err := unpack.SevenZip(t.Context(), slog.Default(), archive, outDir, unpack.Options{})
	if err != nil {
		t.Fatalf("SevenZip error: %v\nOutput:\n%s", err, res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("SevenZip exit code %d\nOutput:\n%s", res.ExitCode, res.Output)
	}
}

// TestUnRAR_OnCommandFiresBeforeExec verifies the OnCommand callback is
// invoked with the full argv before exec runs. Uses a non-existent binary
// so the test doesn't depend on unrar being installed; OnCommand fires
// before cmd.Run, so the test works whether or not unrar is on the host.
func TestUnRAR_OnCommandFiresBeforeExec(t *testing.T) {
	var captured string
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.rar",
		Parts:    []string{"/tmp/does-not-exist.rar"},
	}
	opts := unpack.Options{
		UnrarCommand: "/nonexistent/binary",
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	// Run is expected to fail; we don't check err.
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), archive, t.TempDir(), opts)

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	if !strings.Contains(captured, "/nonexistent/binary") {
		t.Errorf("cmdline missing binary: %q", captured)
	}
	if !strings.Contains(captured, archive.MainFile) {
		t.Errorf("cmdline missing archive path: %q", captured)
	}
	// Must contain the canonical unrar flags so we know it's the real argv,
	// not a stub formatted by the caller.
	for _, flag := range []string{"-y", "-idp", "-scf"} {
		if !strings.Contains(captured, flag) {
			t.Errorf("cmdline missing flag %q: %q", flag, captured)
		}
	}
}

// TestUnRAR_HasProblemDegradedMode verifies that when HasProblem is true
// (non-original or old unrar), the flags -scf, -or, -ai, -tsm- are NOT
// emitted. Matches SABnzbd's RAR_PROBLEM degraded mode.
func TestUnRAR_HasProblemDegradedMode(t *testing.T) {
	var captured string
	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.rar",
		Parts:    []string{"/tmp/does-not-exist.rar"},
	}
	opts := unpack.Options{
		UnrarCommand:     "/nonexistent/binary",
		HasProblem:       true,
		IgnoreUnrarDates: true, // should be suppressed in degraded mode
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), archive, t.TempDir(), opts)

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}

	// These flags should be present (always used):
	for _, flag := range []string{"-y", "-idp", "-o-"} {
		if !strings.Contains(captured, flag) {
			t.Errorf("degraded mode: cmdline missing required flag %q: %q", flag, captured)
		}
	}

	// These flags should be ABSENT in degraded mode:
	for _, flag := range []string{"-scf", "-ai", "-or", "-tsm-"} {
		if strings.Contains(captured, flag) {
			t.Errorf("degraded mode: cmdline should NOT contain flag %q but does: %q", flag, captured)
		}
	}
}

// TestSevenZip_OnCommandFiresBeforeExec mirrors the unrar test for 7z.
func TestSevenZip_OnCommandFiresBeforeExec(t *testing.T) {
	var captured string
	archive := unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.7z",
		Parts:    []string{"/tmp/does-not-exist.7z"},
	}
	opts := unpack.Options{
		SevenZipCommand: "/nonexistent/binary",
		OnCommand: func(cmdLine string) {
			captured = cmdLine
		},
	}
	_, _ = unpack.SevenZip(t.Context(), slog.Default(), archive, t.TempDir(), opts)

	if captured == "" {
		t.Fatal("OnCommand was not called")
	}
	if !strings.Contains(captured, archive.MainFile) {
		t.Errorf("cmdline missing archive path: %q", captured)
	}
}
