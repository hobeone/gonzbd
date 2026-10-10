package unpack

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/hobeone/rarengine"
)

// linkFixtures lists each RAR5 link fixture with the members that must be
// extracted and the link members that must not appear at all.
var linkFixtures = []struct {
	name    string
	regular map[string]string // member -> content
	links   []string
}{
	{"rar5_link_symlink", map[string]string{"real.txt": "real text"}, []string{"link.txt"}},
	{"rar5_link_hard", map[string]string{"orig.txt": "orig content"}, []string{"hard.txt"}},
	{"rar5_link_solid", map[string]string{
		"a.txt": "AAAA first member text, compressible compressible compressible",
		"c.txt": "BBBB third member text, compressible compressible compressible a.txt",
	}, []string{"mid.lnk"}},
	{"rar5_link_escape", map[string]string{"real.txt": "real\n", "after.txt": "ok\n"}, []string{"evil.lnk"}},
	{"rar5_link_filecopy", map[string]string{"orig.txt": "file copy content, stored once and referenced by copy.txt\n"}, []string{"copy.txt"}},
}

// nlink returns the hard-link count of the file at p.
func nlink(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no syscall.Stat_t on this platform")
	}
	return uint64(st.Nlink) //nolint:unconvert // Nlink is uint32 on some platforms
}

// A job holds no links: the pure-Go RAR extractor skips every link member
// (symlink, hard link, file reference), extracts the rest of the set, and
// does not list the skipped member as extracted.
func TestGoUnRAR_LinkMembersAreNeverCreated(t *testing.T) {
	for _, fx := range linkFixtures {
		t.Run(fx.name, func(t *testing.T) {
			var lines []string
			outDir, res, logs, err := extractLinkFixture(t, fx.name, Options{OnLine: func(l string) { lines = append(lines, l) }})
			if err != nil {
				t.Fatalf("a skipped link must not fail the set: %v", err)
			}
			for name, want := range fx.regular {
				if got := mustRead(t, filepath.Join(outDir, name)); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
				if n := nlink(t, filepath.Join(outDir, name)); n != 1 {
					t.Errorf("%s has %d names, want 1", name, n)
				}
			}
			for _, link := range fx.links {
				if _, lerr := os.Lstat(filepath.Join(outDir, link)); !os.IsNotExist(lerr) {
					t.Errorf("link member %s was created (err=%v)", link, lerr)
				}
				for _, f := range res.ExtractedFiles {
					if filepath.Base(f) == link {
						t.Errorf("skipped link %s listed in ExtractedFiles: %v", link, res.ExtractedFiles)
					}
				}
				if !slices.Contains(lines, "Skipping link: "+link) {
					t.Errorf("OnLine did not report skipping %s: %v", link, lines)
				}
				if !strings.Contains(logs, "skipping link entry") || !strings.Contains(logs, "name="+link) {
					t.Errorf("skip of %s not logged:\n%s", link, logs)
				}
			}
		})
	}
}

// A link member never touches what is already in the extraction root, even
// with OverwriteFiles on. DirectUnpack's root is the job directory, so the
// files here stand for downloaded volumes: a hard link from one onto another
// would give a later write through either name the other's bytes.
func TestExtractEntryRarengine_LinkMemberLeavesTheRootUntouched(t *testing.T) {
	for _, lt := range []rarengine.LinkType{
		rarengine.LinkUnixSymlink, rarengine.LinkWindowsSymlink, rarengine.LinkWindowsJunction,
		rarengine.LinkHardLink, rarengine.LinkFileCopy, rarengine.LinkType(99),
	} {
		t.Run(fmt.Sprint("link_type_", int(lt)), func(t *testing.T) {
			outDir := t.TempDir()
			vol1 := filepath.Join(outDir, "job.part01.rar")
			vol2 := filepath.Join(outDir, "job.part02.rar")
			if err := os.WriteFile(vol1, []byte("volume one"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(vol2, []byte("volume two"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(outDir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, nil))
			fh := &rarengine.FileHeader{Name: "job.part02.rar", LinkType: lt, LinkTarget: "job.part01.rar"}
			// A nil reader proves the link path never reads.
			err = ExtractEntryRarengine(context.Background(), root, outDir, "job.part02.rar", vol2, fh, nil, Options{OverwriteFiles: true}, log)
			if err != nil {
				t.Fatalf("a link member must be skipped, not fail: %v", err)
			}
			if got := mustRead(t, vol1); got != "volume one" {
				t.Errorf("link target rewritten: %q", got)
			}
			if got := mustRead(t, vol2); got != "volume two" {
				t.Errorf("existing file at the link's name replaced: %q", got)
			}
			for _, p := range []string{vol1, vol2} {
				if n := nlink(t, p); n != 1 {
					t.Errorf("%s has %d names, want 1", filepath.Base(p), n)
				}
			}
			if ExtractedEntryExists(fh) {
				t.Error("a skipped link member is reported as extracted")
			}
			if !strings.Contains(buf.String(), "skipping link entry") {
				t.Errorf("skip not logged: %s", buf.String())
			}
		})
	}
}

// The external extractors write into a private staging directory, and
// publishStagedExtraction decides what reaches outDir. unrar creates hard
// links and (without -ol-) symlinks there; 7z creates symlinks. Neither may
// reach the job: a symlink is not published, and of several names for one
// inode only the first is.
func TestPublishStagedExtraction_PublishesNoLinks(t *testing.T) {
	outDir := t.TempDir()
	stageDir, err := prepareStageDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()
	if err := os.WriteFile(filepath.Join(stageDir, "a.mkv"), []byte("movie"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(stageDir, "a.mkv"), filepath.Join(stageDir, "b.mkv")); err != nil {
		t.Skipf("filesystem cannot hold a hard link: %v", err)
	}
	if err := os.Symlink("a.mkv", filepath.Join(stageDir, "s.mkv")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(stageDir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	var lines []string
	published, err := publishStagedExtraction(slog.New(slog.DiscardHandler), outDir, stageDir, Options{OnLine: func(l string) { lines = append(lines, l) }})
	if err != nil {
		t.Fatalf("publishStagedExtraction: %v", err)
	}
	if !slices.Equal(published, []string{"a.mkv"}) {
		t.Fatalf("published = %v, want [a.mkv]", published)
	}
	for _, name := range []string{"b.mkv", "s.mkv", "dirlink"} {
		if _, lerr := os.Lstat(filepath.Join(outDir, name)); !os.IsNotExist(lerr) {
			t.Errorf("%s reached outDir (err=%v)", name, lerr)
		}
		if !slices.Contains(lines, "Skipping link: "+name) {
			t.Errorf("OnLine did not report skipping %s: %v", name, lines)
		}
	}
	if err := os.RemoveAll(stageDir); err != nil {
		t.Fatal(err)
	}
	if n := nlink(t, filepath.Join(outDir, "a.mkv")); n != 1 {
		t.Errorf("a.mkv has %d names once the stage is gone, want 1", n)
	}
}

// End to end through the real unrar binary, when installed. Without -ol-
// (version unknown, as for an unrar older than 7.00) unrar creates symlinks
// and hard links in its stage; with it, hard links only. Either way what
// reaches outDir is regular files with one name each. unrar writes a file
// reference as an independent copy, which is not a link and is published.
func TestUnRAR_LinkMembersAreNotPublished(t *testing.T) {
	if _, err := exec.LookPath("unrar"); err != nil {
		t.Skip("unrar not installed")
	}
	versions := []int{0}
	if v := DetectUnrar(context.Background(), "unrar").Version; v >= unrarSkipSymlinksVersion {
		versions = append(versions, v)
	}
	for _, version := range versions {
		for _, fx := range linkFixtures {
			t.Run(fmt.Sprintf("%s/unrar_%d", fx.name, version), func(t *testing.T) {
				checkUnRARPublishesNoLinks(t, fx.name, fx.regular, version)
			})
		}
	}
}

func checkUnRARPublishesNoLinks(t *testing.T, name string, regular map[string]string, version int) {
	t.Helper()
	outDir := t.TempDir()
	archive := Archive{Type: RarArchive, Name: name, MainFile: filepath.Join("testdata", name+".rar")}
	res, err := UnRAR(context.Background(), slog.New(slog.DiscardHandler), archive, outDir, "", Options{UnrarVersion: version})
	if name == "rar5_link_escape" && version < unrarSkipSymlinksVersion {
		// Without -ol-, unrar itself refuses a link that leaves its
		// destination and exits non-zero: the set fails, nothing is published.
		if err == nil {
			t.Fatal("unrar accepted a link escaping the destination")
		}
		if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
			t.Errorf("a failed extraction published %v", entries)
		}
		return
	}
	if err != nil {
		t.Fatalf("UnRAR: %v\n%s", err, res.Output)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		p := filepath.Join(outDir, e.Name())
		fi, lerr := os.Lstat(p)
		if lerr != nil {
			t.Fatal(lerr)
		}
		if !fi.Mode().IsRegular() {
			t.Errorf("%s reached outDir as %v", e.Name(), fi.Mode().Type())
			continue
		}
		if n := nlink(t, p); n != 1 {
			t.Errorf("%s has %d names, want 1", e.Name(), n)
		}
		names = append(names, e.Name())
	}
	if name == "rar5_link_hard" {
		// Two names for one inode in the stage: one is published.
		if len(names) != 1 || mustRead(t, filepath.Join(outDir, names[0])) != "orig content" {
			t.Errorf("published %v, want one name holding the content", names)
		}
		return
	}
	for member, want := range regular {
		if got := mustRead(t, filepath.Join(outDir, member)); got != want {
			t.Errorf("%s = %q, want %q", member, got, want)
		}
	}
}
