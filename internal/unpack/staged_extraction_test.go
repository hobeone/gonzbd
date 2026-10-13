package unpack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dirTree lists every entry under dir, directories included, as sorted
// slash-separated relative paths.
func dirTree(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		names = append(names, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(names)
	return names
}

// copyInto copies the file src into dir under its base name and returns the
// copy's path.
func copyInto(t *testing.T, src, dir string) string {
	t.Helper()
	data, err := os.ReadFile(src) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

// An in-process extraction cancelled after it has written a member leaves
// nothing of itself in the output directory, so a later pass over the same
// directory meets no member of it (#900). The archive sits in the output
// directory, as a job's archives do.
func TestInProcessExtraction_CancelledLeavesNothing(t *testing.T) {
	t.Parallel()

	tarBytes := func(t *testing.T) []byte {
		return buildTarBytes(t, []tarEntry{
			{name: "d/", typeflag: '5'},
			{name: "d/one.txt", content: []byte("one")},
			{name: "two.txt", content: []byte("two")},
		})
	}
	engines := []struct {
		name    string
		archive func(t *testing.T, dir string) Archive
		run     func(ctx context.Context, a Archive, outDir string, opts Options) (Result, error)
	}{
		{
			name: "go_unrar",
			archive: func(t *testing.T, dir string) Archive {
				p := copyInto(t, filepath.Join("testdata", "single_rar5.rar"), dir)
				return Archive{Type: RarArchive, Name: "single_rar5", MainFile: p, Parts: []string{p}}
			},
			run: func(ctx context.Context, a Archive, outDir string, opts Options) (Result, error) {
				return GoUnRAR(ctx, testLogger(), a, outDir, "", opts)
			},
		},
		{
			name: "go_7z",
			archive: func(t *testing.T, dir string) Archive {
				p := copyInto(t, filepath.Join("testdata", "flat_collide.7z"), dir)
				return Archive{Type: SevenZipArchive, Name: "flat_collide", MainFile: p, Parts: []string{p}}
			},
			run: func(ctx context.Context, a Archive, outDir string, opts Options) (Result, error) {
				return GoSevenZip(ctx, testLogger(), a, outDir, "", opts)
			},
		},
		{
			name: "go_tar",
			archive: func(t *testing.T, dir string) Archive {
				p := filepath.Join(dir, "set.tar")
				if err := os.WriteFile(p, tarBytes(t), 0o600); err != nil {
					t.Fatal(err)
				}
				return Archive{Type: TarArchive, Name: "set", MainFile: p, Parts: []string{p}}
			},
			run: func(ctx context.Context, a Archive, outDir string, opts Options) (Result, error) {
				return GoTar(ctx, testLogger(), a, outDir, opts)
			},
		},
	}
	for _, e := range engines {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			outDir := t.TempDir()
			a := e.archive(t, outDir)
			before := dirTree(t, outDir)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var extracted int
			opts := Options{OnLine: func(line string) {
				if strings.HasPrefix(line, "Extracting  ") {
					extracted++
					cancel()
				}
			}}
			_, err := e.run(ctx, a, outDir, opts)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if extracted == 0 {
				t.Fatal("no member was extracted before the cancellation; the test proves nothing")
			}
			if got := dirTree(t, outDir); !slices.Equal(got, before) {
				t.Errorf("output dir after a cancelled extraction = %q, want only %q", got, before)
			}
		})
	}
}

// A failed in-process extraction with OverwriteFiles leaves a file it would
// have replaced as it was, and removes every directory it created (#900).
func TestGoTar_FailedExtractionChangesNothing(t *testing.T) {
	t.Parallel()
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "a.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Two good members, then a block that is not a tar header: tar.Reader
	// returns ErrHeader at the third Next.
	data := buildTarBytes(t, []tarEntry{
		{name: "a.txt", content: []byte("new")},
		{name: "d/f.txt", content: []byte("f")},
	})
	data = data[:len(data)-1024] // drop the two zero end-of-archive blocks
	data = append(data, []byte(strings.Repeat("x", 512))...)
	p := filepath.Join(outDir, "set.tar")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before := dirTree(t, outDir)

	_, err := GoTar(context.Background(), testLogger(), Archive{Type: TarArchive, Name: "set", MainFile: p, Parts: []string{p}}, outDir, Options{OverwriteFiles: true})
	if err == nil {
		t.Fatal("GoTar succeeded on a damaged tar")
	}
	if got := dirTree(t, outDir); !slices.Equal(got, before) {
		t.Errorf("output dir after a failed extraction = %q, want %q", got, before)
	}
	got, readErr := os.ReadFile(filepath.Join(outDir, "a.txt")) //nolint:gosec // test path
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "old" {
		t.Errorf("a.txt = %q after a failed extraction, want the file it had, %q", got, "old")
	}
}
