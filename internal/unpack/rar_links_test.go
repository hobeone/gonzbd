package unpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	outDir, res, _, err := extractLinkFixture(t, "rar5_link_symlink", Options{ExtractSymlinks: true})
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
	outDir, _, _, err := extractLinkFixture(t, "rar5_link_solid", Options{ExtractSymlinks: true})
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
	outDir, res, logs, err := extractLinkFixture(t, "rar5_link_escape", Options{ExtractSymlinks: true, OnLine: func(l string) { lines = append(lines, l) }})
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
	for _, want := range []string{"32 MiB", "not corruption", "go_rar_fallback", "unrar is installed", "big.bin", "268435456"} {
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
		opts.ExtractSymlinks = true
		opts.Symlinks = NewSymlinkBatch()
		if err := ExtractEntryRarengine(context.Background(), root, outDir, "l", filepath.Join(outDir, "l"), fh, nil, opts, log); err != nil {
			t.Fatal(err)
		}
		if _, err := opts.Symlinks.Finish(root, opts, log); err != nil {
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

func TestGoUnRAR_SymlinkMembersSkippedByDefault(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		link    string
		other   string
	}{
		{"rar5_link_symlink", "link.txt", "real.txt"},
		{"rar5_link_solid", "mid.lnk", "c.txt"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			var lines []string
			outDir, res, logs, err := extractLinkFixture(t, tc.fixture, Options{OnLine: func(l string) { lines = append(lines, l) }})
			if err != nil {
				t.Fatalf("a skipped symlink must not fail the set: %v", err)
			}
			if _, lerr := os.Lstat(filepath.Join(outDir, tc.link)); !os.IsNotExist(lerr) {
				t.Fatalf("%s was created with extract_symlinks off (err=%v)", tc.link, lerr)
			}
			if _, serr := os.Stat(filepath.Join(outDir, tc.other)); serr != nil {
				t.Errorf("neighbouring member %s missing: %v", tc.other, serr)
			}
			want := "go_unrar: skipping symlink member " + tc.link + ": extract_symlinks is off"
			if !strings.Contains(logs, want) {
				t.Errorf("log lacks %q:\n%s", want, logs)
			}
			for _, f := range res.ExtractedFiles {
				if strings.HasSuffix(f, tc.link) {
					t.Errorf("skipped symlink listed in ExtractedFiles: %v", res.ExtractedFiles)
				}
			}
			if !strings.Contains(strings.Join(lines, "\n"), "extract_symlinks is off") {
				t.Errorf("OnLine did not report the skip: %v", lines)
			}
		})
	}
}

func TestGoUnRAR_HardLinkStillExtractedWithSymlinksOff(t *testing.T) {
	outDir, _, _, err := extractLinkFixture(t, "rar5_link_hard", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(outDir, "hard.txt")); got != "orig content" {
		t.Errorf("hard.txt = %q", got)
	}
}

// linkSession is a root plus a batch, for driving ExtractEntryRarengine with
// hand-built headers.
type linkSession struct {
	t      *testing.T
	dir    string
	root   *os.Root
	opts   Options
	logs   *bytes.Buffer
	notes  []string
	finish []string
}

func newLinkSession(t *testing.T, opts Options) *linkSession {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	ls := &linkSession{t: t, dir: dir, root: root, logs: &bytes.Buffer{}}
	opts.ExtractSymlinks = true
	opts.Symlinks = NewSymlinkBatch()
	opts.OnLine = func(l string) { ls.notes = append(ls.notes, l) }
	ls.opts = opts
	return ls
}

func (ls *linkSession) log() *slog.Logger {
	return slog.New(slog.NewTextHandler(ls.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (ls *linkSession) add(name string, lt rarengine.LinkType, target string) {
	ls.t.Helper()
	fh := &rarengine.FileHeader{Name: name, LinkType: lt, LinkTarget: target}
	if err := ExtractEntryRarengine(context.Background(), ls.root, ls.dir, name, filepath.Join(ls.dir, name), fh, nil, ls.opts, ls.log()); err != nil {
		ls.t.Fatalf("ExtractEntryRarengine(%s): %v", name, err)
	}
}

func (ls *linkSession) done() []string {
	ls.t.Helper()
	got, err := ls.opts.Symlinks.Finish(ls.root, ls.opts, ls.log())
	if err != nil {
		ls.t.Fatalf("Finish: %v", err)
	}
	ls.finish = got
	return got
}

func (ls *linkSession) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(ls.dir, name))
	return err == nil
}

// A later member can make an earlier, already-valid link escape: "link" ->
// "sub/d/../../z" is inside the root while sub/d does not exist (it cleans to
// "z"), but once the archive's later member "sub/d" -> ".." is created, sub/d
// is the root and the same target climbs out of it. Creation-time checking
// alone accepts both; the final re-validation must remove the first.
func TestSymlinkBatch_LaterLinkMakesEarlierLinkEscape(t *testing.T) {
	ls := newLinkSession(t, Options{})
	ls.add("link", rarengine.LinkUnixSymlink, "sub/d/../../z")
	ls.add("sub/d", rarengine.LinkUnixSymlink, "..")
	got := ls.done()

	if ls.exists("link") {
		t.Fatal("link survived although sub/d now makes it resolve outside the root")
	}
	if !ls.exists("sub/d") {
		t.Error("the harmless link sub/d should remain")
	}
	if len(got) != 1 || got[0] != "sub/d" {
		t.Errorf("Finish returned %v, want [sub/d]", got)
	}
	if !strings.Contains(ls.logs.String(), "removed symlink that escapes") {
		t.Errorf("removal not logged:\n%s", ls.logs.String())
	}
}

// The same hole through OverwriteFiles: "sub/f" is a regular file when the
// first link is recorded, then a later member replaces it with a symlink to
// "..".
func TestSymlinkBatch_FileReplacedBySymlinkMakesEarlierLinkEscape(t *testing.T) {
	ls := newLinkSession(t, Options{OverwriteFiles: true})
	if err := os.MkdirAll(filepath.Join(ls.dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ls.dir, "sub", "f"), []byte("regular"), 0o600); err != nil {
		t.Fatal(err)
	}
	ls.add("link", rarengine.LinkUnixSymlink, "sub/f/../../z")
	ls.add("sub/f", rarengine.LinkUnixSymlink, "..")
	ls.done()

	if ls.exists("link") {
		t.Fatal("link survived after sub/f was replaced by a symlink to the root")
	}
	assertSymlink(t, filepath.Join(ls.dir, "sub", "f"), "..")
}

func TestSymlinkBatch_NothingCreatedBeforeFinish(t *testing.T) {
	ls := newLinkSession(t, Options{})
	ls.add("l", rarengine.LinkUnixSymlink, "t")
	if ls.exists("l") {
		t.Fatal("symlink created before the archive's last member")
	}
	if got := ls.done(); len(got) != 1 || got[0] != "l" {
		t.Fatalf("Finish = %v", got)
	}
	assertSymlink(t, filepath.Join(ls.dir, "l"), "t")
}

func TestSymlinkMember_OneFolderRefusesTargetsOutsideFlatLayout(t *testing.T) {
	ls := newLinkSession(t, Options{OneFolder: true})
	ls.add("a/ok", rarengine.LinkUnixSymlink, "sibling.txt")
	ls.add("bad1", rarengine.LinkUnixSymlink, "sub/x")
	ls.add("bad2", rarengine.LinkUnixSymlink, "../x")
	ls.add("bad3", rarengine.LinkUnixSymlink, "./sub/../x/y")
	ls.done()
	if !ls.exists("a/ok") && !ls.exists("ok") {
		t.Error("bare sibling target should be accepted")
	}
	for _, n := range []string{"bad1", "bad2", "bad3"} {
		if ls.exists(n) {
			t.Errorf("%s created although its target leaves the flattened layout", n)
		}
	}
	if !strings.Contains(ls.logs.String(), "flattened layout") {
		t.Errorf("refusal reason not logged:\n%s", ls.logs.String())
	}
}

func TestLinkMembers_NameTooLongIsARefusalNotAFailure(t *testing.T) {
	long := strings.Repeat("n", 300)
	ls := newLinkSession(t, Options{})
	if err := os.WriteFile(filepath.Join(ls.dir, "orig.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	ls.add(long, rarengine.LinkUnixSymlink, "orig.txt") // Finish must not fail
	ls.add("fine", rarengine.LinkUnixSymlink, "orig.txt")
	ls.add(long+"h", rarengine.LinkHardLink, "orig.txt") // ExtractEntryRarengine must not fail
	got := ls.done()
	if len(got) != 1 || got[0] != "fine" {
		t.Errorf("Finish = %v, want only the link that fits", got)
	}
	if !strings.Contains(ls.logs.String(), "destination cannot hold this link") {
		t.Errorf("refusal reason not logged:\n%s", ls.logs.String())
	}
}

func TestIsLinkUnsupported(t *testing.T) {
	for _, e := range []error{syscall.EPERM, syscall.ENOTSUP, syscall.ENAMETOOLONG,
		&os.PathError{Op: "symlinkat", Path: "x", Err: syscall.EPERM}} {
		if !isLinkUnsupported(e) {
			t.Errorf("%v not treated as unsupported", e)
		}
	}
	for _, e := range []error{syscall.ENOSPC, syscall.EACCES, errors.New("x")} {
		if isLinkUnsupported(e) {
			t.Errorf("%v wrongly treated as unsupported", e)
		}
	}
}

// A target read back from disk is literal. On Unix a backslash is an ordinary
// name character, so "x\..\.." is one component naming nothing, not a climb.
func TestPhysicalResolve_OnDiskTargetsKeepBackslashes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(`x\..\..`, filepath.Join(dir, "s")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := physicalResolve(root, "s/y"); err != nil {
		t.Fatalf("on-disk backslash target was rewritten into a climb: %v", err)
	}
	// An archive target about to be stored is still normalized.
	if _, err := normalizeLinkTarget(`..\..\z`); err != nil {
		t.Fatal(err)
	}
	if got, _ := normalizeLinkTarget(`a\b`); got != "a/b" {
		t.Errorf("archive target not normalized: %q", got)
	}
}

// rarengine verifies a plain BLAKE2sp digest, so a damaged -htb archive is
// caught by the Go path as corruption (it extracted silently before).
func TestGoUnRAR_DamagedBlake2spArchiveIsCorrupt(t *testing.T) {
	outDir := t.TempDir()
	archive := Archive{
		Type:     RarArchive,
		Name:     "damaged_b2",
		MainFile: filepath.Join("..", "..", "test", "fixtures", "par2", "layout_b", "damaged_b2.rar"),
	}
	res, err := GoUnRAR(context.Background(), slog.New(slog.DiscardHandler), archive, outDir, "", Options{})
	if err == nil {
		t.Fatal("damaged BLAKE2sp archive extracted without an error")
	}
	if !errors.Is(err, rarengine.ErrCRCMismatch) || res.Reason != FailCorrupt {
		t.Fatalf("err = %v, reason = %v; want ErrCRCMismatch and FailCorrupt", err, res.Reason)
	}
}

// The final check also joins the link's real parent and its target lexically.
// "hop/../../x" stays inside the root when walked physically (hop -> sub/deeper),
// but lexically it climbs out, and a link that only passes one of the two
// checks is removed.
func TestSymlinkBatch_FinalCheckIsLexicallyConservative(t *testing.T) {
	ls := newLinkSession(t, Options{})
	if err := os.MkdirAll(filepath.Join(ls.dir, "sub", "deeper"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/deeper", filepath.Join(ls.dir, "hop")); err != nil {
		t.Fatal(err)
	}
	ls.add("l", rarengine.LinkUnixSymlink, "hop/../../x")
	ls.add("fine", rarengine.LinkUnixSymlink, "hop/y")
	got := ls.done()
	if ls.exists("l") {
		t.Error("link that climbs out lexically was kept")
	}
	if !slices.Equal(got, []string{"fine"}) {
		t.Errorf("Finish = %v, want [fine]", got)
	}
}
