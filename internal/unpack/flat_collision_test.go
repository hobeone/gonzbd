package unpack

import (
	"archive/tar"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// flatCase is one archive shape whose members collide once flattened. The
// archives are flat_collide.* (a/f.txt="one", b/f.txt="two") and
// flat_dir_file.* (file a/x="file", directory b/x holding b/x/y="y"); see
// scripts/gen_test_archives.sh.
type flatCase struct {
	name    string
	fixture string // flat_collide or flat_dir_file
	taken   bool   // outDir already holds f.txt="pre"
	want    []string
}

var flatCases = []flatCase{
	{name: "file collides with file", fixture: "flat_collide", want: []string{"one", "two"}},
	{name: "file collides with directory", fixture: "flat_dir_file", want: []string{"file", "y"}},
	{name: "file collides with the output directory", fixture: "flat_collide", taken: true, want: []string{"one", "pre", "two"}},
}

type flatEngine func(ctx context.Context, log *slog.Logger, archive Archive, outDir string, opts Options) (Result, error)

// runFlatCases runs engine over every flatCase with OneFolder on and
// OverwriteFiles off, and requires that every member reached outDir as a
// regular file of its own, that no directory was created, and that the
// Result lists each new file.
func runFlatCases(t *testing.T, engine flatEngine, archiveFor func(t *testing.T, fixture string) Archive) {
	t.Helper()
	for _, tc := range flatCases {
		t.Run(tc.name, func(t *testing.T) {
			outDir := t.TempDir()
			if tc.taken {
				if err := os.WriteFile(filepath.Join(outDir, "f.txt"), []byte("pre"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			res, err := engine(t.Context(), testLogger(), archiveFor(t, tc.fixture), outDir, Options{OneFolder: true})
			if err != nil {
				t.Fatalf("extract: %v\noutput:\n%s", err, res.Output)
			}
			contents, dirs := flatOutput(t, outDir)
			if !slices.Equal(contents, tc.want) {
				t.Errorf("contents of the regular files in outDir = %q, want %q (every member kept)", contents, tc.want)
			}
			if len(dirs) != 0 {
				t.Errorf("flat extraction created directories %q, want none", dirs)
			}
			wantListed := len(tc.want)
			if tc.taken {
				wantListed--
				if got, err := os.ReadFile(filepath.Join(outDir, "f.txt")); err != nil || string(got) != "pre" {
					t.Errorf("the file outDir already held: %q, %v; want %q kept", got, err, "pre")
				}
			}
			if len(res.ExtractedFiles) != wantListed {
				t.Errorf("ExtractedFiles = %q, want %d entries", res.ExtractedFiles, wantListed)
			}
		})
	}
}

// flatOutput returns the sorted contents of every regular file under dir and
// the relative path of every directory under it.
func flatOutput(t *testing.T, dir string) (contents, dirs []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, rel)
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // path is under the test's own temp dir
		if err != nil {
			return err
		}
		contents = append(contents, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(contents)
	return contents, dirs
}

func fixtureArchive(typ ArchiveType, ext string) func(t *testing.T, fixture string) Archive {
	return func(t *testing.T, fixture string) Archive {
		t.Helper()
		p, err := filepath.Abs(filepath.Join("testdata", fixture+ext))
		if err != nil {
			t.Fatal(err)
		}
		return Archive{Type: typ, Name: fixture, MainFile: p, Parts: []string{p}}
	}
}

func requireBinary(t *testing.T, find func(Options) (string, error)) {
	t.Helper()
	bin, err := find(Options{})
	if err == nil {
		_, err = exec.LookPath(bin)
	}
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("extraction binary not found in CI: %v", err)
		}
		t.Skipf("extraction binary not found: %v", err)
	}
}

func TestFlatCollisions_UnRAR(t *testing.T) {
	requireBinary(t, UnrarBin)
	runFlatCases(t, func(ctx context.Context, log *slog.Logger, a Archive, outDir string, opts Options) (Result, error) {
		return UnRAR(ctx, log, a, outDir, "", opts)
	}, fixtureArchive(RarArchive, ".rar"))
}

func TestFlatCollisions_SevenZip(t *testing.T) {
	requireBinary(t, SevenZipBin)
	runFlatCases(t, func(ctx context.Context, log *slog.Logger, a Archive, outDir string, opts Options) (Result, error) {
		return SevenZip(ctx, log, a, outDir, "", opts)
	}, fixtureArchive(SevenZipArchive, ".7z"))
}

func TestFlatCollisions_GoUnRAR(t *testing.T) {
	runFlatCases(t, func(ctx context.Context, log *slog.Logger, a Archive, outDir string, opts Options) (Result, error) {
		return GoUnRAR(ctx, log, a, outDir, "", opts)
	}, fixtureArchive(RarArchive, ".rar"))
}

func TestFlatCollisions_GoSevenZip(t *testing.T) {
	runFlatCases(t, func(ctx context.Context, log *slog.Logger, a Archive, outDir string, opts Options) (Result, error) {
		return GoSevenZip(ctx, log, a, outDir, "", opts)
	}, fixtureArchive(SevenZipArchive, ".7z"))
}

// TestFlatCollisions_GoTar builds the two shapes as tars in the order `tar c`
// writes them, each directory before its contents, so the directory b/x
// comes after the file a/x has taken the name x.
func TestFlatCollisions_GoTar(t *testing.T) {
	shapes := map[string][]tarEntry{
		"flat_collide": {
			{name: "a/", typeflag: tar.TypeDir},
			{name: "a/f.txt", content: []byte("one")},
			{name: "b/", typeflag: tar.TypeDir},
			{name: "b/f.txt", content: []byte("two")},
		},
		"flat_dir_file": {
			{name: "a/", typeflag: tar.TypeDir},
			{name: "a/x", content: []byte("file")},
			{name: "b/", typeflag: tar.TypeDir},
			{name: "b/x/", typeflag: tar.TypeDir},
			{name: "b/x/y", content: []byte("y")},
		},
	}
	runFlatCases(t, GoTar, func(t *testing.T, fixture string) Archive {
		t.Helper()
		p := buildTar(t, t.TempDir(), fixture+".tar", shapes[fixture])
		return Archive{Type: TarArchive, Name: fixture, MainFile: p, Parts: []string{p}}
	})
}
