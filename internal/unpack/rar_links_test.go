package unpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/rarengine"
)

func extractLinkFixture(t *testing.T, name string, opts Options) (outDir string, res Result, logs string, err error) {
	t.Helper()
	outDir = t.TempDir()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	archive := Archive{
		Type:     RarArchive,
		Name:     name,
		MainFile: filepath.Join("testdata", name+".rar"),
	}
	res, err = GoUnRAR(context.Background(), log, archive, outDir, "", opts)
	return outDir, res, buf.String(), err
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func assertSymlink(t *testing.T, p, wantTarget string) {
	t.Helper()
	got, err := os.Readlink(p)
	if err != nil {
		t.Fatalf("readlink %s: %v", p, err)
	}
	if got != wantTarget {
		t.Fatalf("symlink %s -> %q, want %q", p, got, wantTarget)
	}
}

func TestGoUnRAR_SymlinkMember(t *testing.T) {
	outDir, res, _, err := extractLinkFixture(t, "rar5_link_symlink", Options{})
	if err != nil {
		t.Fatalf("GoUnRAR: %v", err)
	}
	if got := mustRead(t, filepath.Join(outDir, "real.txt")); got != "real text" {
		t.Errorf("real.txt = %q", got)
	}
	assertSymlink(t, filepath.Join(outDir, "link.txt"), "real.txt")
	if got := mustRead(t, filepath.Join(outDir, "link.txt")); got != "real text" {
		t.Errorf("content through link = %q", got)
	}
	if len(res.ExtractedFiles) != 2 {
		t.Errorf("ExtractedFiles = %v, want real.txt and link.txt", res.ExtractedFiles)
	}
}

func TestGoUnRAR_HardLinkMember(t *testing.T) {
	outDir, _, _, err := extractLinkFixture(t, "rar5_link_hard", Options{})
	if err != nil {
		t.Fatalf("GoUnRAR: %v", err)
	}
	if got := mustRead(t, filepath.Join(outDir, "orig.txt")); got != "orig content" {
		t.Errorf("orig.txt = %q", got)
	}
	if got := mustRead(t, filepath.Join(outDir, "hard.txt")); got != "orig content" {
		t.Errorf("hard.txt = %q", got)
	}
	a, errA := os.Stat(filepath.Join(outDir, "orig.txt"))
	b, errB := os.Stat(filepath.Join(outDir, "hard.txt"))
	if errA != nil || errB != nil {
		t.Fatalf("stat: %v %v", errA, errB)
	}
	if !os.SameFile(a, b) {
		t.Error("hard.txt is not a hard link to orig.txt")
	}
}

func TestGoUnRAR_SolidArchiveWithLink(t *testing.T) {
	outDir, _, _, err := extractLinkFixture(t, "rar5_link_solid", Options{})
	if err != nil {
		t.Fatalf("GoUnRAR: %v", err)
	}
	if got := mustRead(t, filepath.Join(outDir, "a.txt")); got != "AAAA first member text, compressible compressible compressible" {
		t.Errorf("a.txt = %q", got)
	}
	assertSymlink(t, filepath.Join(outDir, "mid.lnk"), "a.txt")
	if got := mustRead(t, filepath.Join(outDir, "c.txt")); got != "BBBB third member text, compressible compressible compressible a.txt" {
		t.Errorf("c.txt = %q", got)
	}
}

func TestGoUnRAR_EscapingSymlinkRefused(t *testing.T) {
	var lines []string
	outDir, res, logs, err := extractLinkFixture(t, "rar5_link_escape", Options{OnLine: func(l string) { lines = append(lines, l) }})
	if err != nil {
		t.Fatalf("a refused link must not fail the set: %v", err)
	}
	if _, lerr := os.Lstat(filepath.Join(outDir, "evil.lnk")); !os.IsNotExist(lerr) {
		t.Fatalf("evil.lnk was created (err=%v)", lerr)
	}
	if got := mustRead(t, filepath.Join(outDir, "real.txt")); got != "real\n" {
		t.Errorf("real.txt = %q", got)
	}
	if got := mustRead(t, filepath.Join(outDir, "after.txt")); got != "ok\n" {
		t.Errorf("after.txt = %q", got)
	}
	if !strings.Contains(logs, "skipping link entry") || !strings.Contains(logs, "climbs out of the extraction root") {
		t.Errorf("refusal not logged with a reason:\n%s", logs)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Skipping link: evil.lnk") {
		t.Errorf("OnLine did not report the skip: %v", lines)
	}
	for _, f := range res.ExtractedFiles {
		if strings.HasSuffix(f, "evil.lnk") {
			t.Errorf("refused link listed in ExtractedFiles: %v", res.ExtractedFiles)
		}
	}
}

func TestClassifyRarEngineError_DictionaryTooLarge(t *testing.T) {
	for _, err := range []error{
		rarengine.ErrDictionaryTooLarge,
		fmt.Errorf("go_unrar: write big.bin: %w", rarengine.ErrDictionaryTooLarge),
	} {
		got := ClassifyRarEngineError(err)
		if got != FailDictionaryTooLarge {
			t.Errorf("ClassifyRarEngineError(%v) = %v, want FailDictionaryTooLarge", err, got)
		}
		if got == FailCorrupt || got == FailUnknown {
			t.Errorf("dictionary limit classed as %v", got)
		}
	}
	if !strings.Contains(FailDictionaryTooLarge.String(), "dictionary") {
		t.Errorf("String() = %q does not name the dictionary", FailDictionaryTooLarge.String())
	}
}

func TestNoteDictionaryLimit(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	fh := &rarengine.FileHeader{Name: "big.bin", DictSize: 256 << 20}

	NoteDictionaryLimit(log, fmt.Errorf("x: %w", rarengine.ErrDictionaryTooLarge), fh)
	out := buf.String()
	for _, want := range []string{"32 MiB", "not corruption", "big.bin", "268435456"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}

	buf.Reset()
	NoteDictionaryLimit(log, rarengine.ErrCRCMismatch, fh)
	NoteDictionaryLimit(log, nil, fh)
	if buf.Len() != 0 {
		t.Errorf("unrelated error logged: %q", buf.String())
	}
}

func TestResolveSymlinkTarget(t *testing.T) {
	setup := func(t *testing.T) *os.Root {
		t.Helper()
		dir := t.TempDir()
		for _, d := range []string{"a", "sub/deeper"} {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
				t.Fatal(err)
			}
		}
		// a/d -> .. : a/d is the extraction root itself.
		if err := os.Symlink("..", filepath.Join(dir, "a", "d")); err != nil {
			t.Fatal(err)
		}
		// hop -> sub/deeper
		if err := os.Symlink("sub/deeper", filepath.Join(dir, "hop")); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { root.Close() })
		return root
	}

	tests := []struct {
		name    string
		destRel string
		target  string
		want    string // "" means refused
	}{
		{"sibling", "dir/link", "other.txt", "other.txt"},
		{"parent climb within root", "sub/deeper/link", "../../x", "../../x"},
		{"absolute", "link", "/etc/passwd", ""},
		{"windows drive", "link", `C:\Windows\x`, ""},
		{"windows backslash relative", "sub/link", `..\x`, "../x"},
		{"dotdot escape", "link", "../../etc/passwd", ""},
		{"dotdot escape from subdir", "sub/link", "../../x", ""},
		{"empty", "link", "", ""},
		{"nul", "link", "a\x00b", ""},
		// a/d is a symlink to the root, so a/d/f's real directory is the
		// root and "../../z" leaves it. Lexically it cleans to "z".
		{"escape via existing symlink parent", "a/d/f", "../../z", ""},
		{"escape via symlink in target", "link", "a/d/../../z", ""},
		// hop -> sub/deeper (depth 2): "hop/f" lives two deep, so "../../x" is root/x.
		{"symlink to deeper dir is fine", "hop/f", "../../x", "../../x"},
		{"through a symlink to a sibling", "link", "hop/file", "hop/file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := setup(t)
			got, err := resolveSymlinkTarget(root, tt.destRel, tt.target)
			if tt.want == "" {
				if !errors.Is(err, errLinkRefused) {
					t.Fatalf("got (%q, %v), want a refusal", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got (%q, %v), want %q", got, err, tt.want)
			}
		})
	}
}

func TestResolveSymlinkTarget_Loop(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("l2", filepath.Join(dir, "l1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("l1", filepath.Join(dir, "l2")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := resolveSymlinkTarget(root, "link", "l1/x"); !errors.Is(err, errLinkRefused) {
		t.Fatalf("symlink loop: got %v, want refusal", err)
	}
}

func TestExtractEntryRarengine_HardLinkTargetMissing(t *testing.T) {
	outDir := t.TempDir()
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	fh := &rarengine.FileHeader{Name: "h.txt", LinkType: rarengine.LinkHardLink, LinkTarget: "not-there.txt"}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	// A nil reader proves the link path never reads.
	err = ExtractEntryRarengine(context.Background(), root, outDir, "h.txt", filepath.Join(outDir, "h.txt"), fh, nil, Options{}, log)
	if err != nil {
		t.Fatalf("missing target must be skipped, got %v", err)
	}
	if _, lerr := os.Lstat(filepath.Join(outDir, "h.txt")); !os.IsNotExist(lerr) {
		t.Errorf("h.txt exists: %v", lerr)
	}
	if !strings.Contains(buf.String(), "not extracted") {
		t.Errorf("skip not logged: %s", buf.String())
	}
}

func TestExtractEntryRarengine_FileCopyAndHardLinkEscape(t *testing.T) {
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "orig.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	ctx := context.Background()

	copyFH := &rarengine.FileHeader{Name: "sub/copy.txt", LinkType: rarengine.LinkFileCopy, LinkTarget: "orig.txt"}
	if err := ExtractEntryRarengine(ctx, root, outDir, "sub/copy.txt", filepath.Join(outDir, "sub/copy.txt"), copyFH, nil, Options{}, log); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(outDir, "sub", "copy.txt")); got != "data" {
		t.Errorf("copy = %q", got)
	}
	a, _ := os.Stat(filepath.Join(outDir, "orig.txt"))
	b, _ := os.Stat(filepath.Join(outDir, "sub", "copy.txt"))
	if os.SameFile(a, b) {
		t.Error("a file copy must not share an inode with its source")
	}

	evil := &rarengine.FileHeader{Name: "h", LinkType: rarengine.LinkHardLink, LinkTarget: "../../etc/passwd"}
	if err := ExtractEntryRarengine(ctx, root, outDir, "h", filepath.Join(outDir, "h"), evil, nil, Options{}, log); err != nil {
		t.Fatalf("refusal must not be an error: %v", err)
	}
	if _, lerr := os.Lstat(filepath.Join(outDir, "h")); !os.IsNotExist(lerr) {
		t.Errorf("escaping hard link created: %v", lerr)
	}
}

func TestExtractEntryRarengine_SymlinkOverwrite(t *testing.T) {
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "l"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(outDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	fh := &rarengine.FileHeader{Name: "l", LinkType: rarengine.LinkUnixSymlink, LinkTarget: "t"}
	run := func(opts Options) {
		t.Helper()
		if err := ExtractEntryRarengine(context.Background(), root, outDir, "l", filepath.Join(outDir, "l"), fh, nil, opts, log); err != nil {
			t.Fatal(err)
		}
	}
	run(Options{}) // existing file is kept
	if got := mustRead(t, filepath.Join(outDir, "l")); got != "old" {
		t.Fatalf("OverwriteFiles=false replaced the file: %q", got)
	}
	run(Options{OverwriteFiles: true})
	assertSymlink(t, filepath.Join(outDir, "l"), "t")
}
