package unpack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Every extractor names itself in Result.Engine, on success and on failure,
// and lists what it extracted relative to outDir (#911).
func TestExtractors_ResultNamesEngineAndRelativeFiles(t *testing.T) {
	t.Parallel()

	tarFixture := func(t *testing.T, dir string) Archive {
		p := filepath.Join(dir, "set.tar")
		if err := os.WriteFile(p, buildTarBytes(t, []tarEntry{{name: "d/f.txt", content: []byte("f")}}), 0o600); err != nil {
			t.Fatal(err)
		}
		return Archive{Type: TarArchive, Name: "set", MainFile: p, Parts: []string{p}}
	}
	rarFixture := func(t *testing.T, dir string) Archive {
		p := copyInto(t, filepath.Join("testdata", "single_rar5.rar"), dir)
		return Archive{Type: RarArchive, Name: "single_rar5", MainFile: p, Parts: []string{p}}
	}
	sevenZipFixture := func(t *testing.T, dir string) Archive {
		p := copyInto(t, filepath.Join("testdata", "flat_collide.7z"), dir)
		return Archive{Type: SevenZipArchive, Name: "flat_collide", MainFile: p, Parts: []string{p}}
	}
	splitFixture := func(t *testing.T, dir string) Archive {
		parts := make([]string, 0, 2)
		for i, s := range []string{"one", "two"} {
			p := filepath.Join(dir, "joined.bin."+partSuffix(i+1))
			if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, p)
		}
		return Archive{Type: SplitArchive, Name: "joined.bin", MainFile: parts[0], Parts: parts}
	}

	ctx := context.Background()
	engines := []struct {
		engine  string
		binary  string // skip when this binary is not on PATH
		archive func(t *testing.T, dir string) Archive
		run     func(a Archive, outDir string) (Result, error)
	}{
		{"go_unrar", "", rarFixture, func(a Archive, outDir string) (Result, error) {
			return GoUnRAR(ctx, testLogger(), a, outDir, "", Options{})
		}},
		{"unrar", "unrar", rarFixture, func(a Archive, outDir string) (Result, error) {
			return UnRAR(ctx, testLogger(), a, outDir, "", Options{})
		}},
		{"go_7z", "", sevenZipFixture, func(a Archive, outDir string) (Result, error) {
			return GoSevenZip(ctx, testLogger(), a, outDir, "", Options{})
		}},
		{"7z", "7z", sevenZipFixture, func(a Archive, outDir string) (Result, error) {
			return SevenZip(ctx, testLogger(), a, outDir, "", Options{SevenZipCommand: "7z"})
		}},
		{"go_tar", "", tarFixture, func(a Archive, outDir string) (Result, error) {
			return GoTar(ctx, testLogger(), a, outDir, Options{})
		}},
		{"filejoin", "", splitFixture, func(a Archive, outDir string) (Result, error) {
			return FileJoin(ctx, testLogger(), a, outDir, Options{})
		}},
	}
	for _, e := range engines {
		t.Run(e.engine, func(t *testing.T) {
			t.Parallel()
			if e.binary != "" {
				if _, err := exec.LookPath(e.binary); err != nil {
					t.Skipf("%s not on PATH", e.binary)
				}
			}
			outDir := t.TempDir()
			res, err := e.run(e.archive(t, outDir), outDir)
			if err != nil {
				t.Fatalf("extraction failed: %v", err)
			}
			if res.Engine != e.engine {
				t.Errorf("Engine = %q on success, want %q", res.Engine, e.engine)
			}
			if len(res.ExtractedFiles) == 0 {
				t.Fatal("ExtractedFiles is empty")
			}
			for _, f := range res.ExtractedFiles {
				if filepath.IsAbs(f) {
					t.Errorf("ExtractedFiles holds %q, want a path relative to outDir", f)
					continue
				}
				if _, statErr := os.Stat(filepath.Join(outDir, f)); statErr != nil {
					t.Errorf("ExtractedFiles names %q, which is not under outDir: %v", f, statErr)
				}
			}

			missing := Archive{Type: SplitArchive, Name: "gone", MainFile: filepath.Join(outDir, "gone"), Parts: []string{filepath.Join(outDir, "gone")}}
			res, err = e.run(missing, t.TempDir())
			if err == nil {
				t.Fatal("extracting a missing archive succeeded")
			}
			if res.Engine != e.engine {
				t.Errorf("Engine = %q on failure, want %q", res.Engine, e.engine)
			}
		})
	}
}
