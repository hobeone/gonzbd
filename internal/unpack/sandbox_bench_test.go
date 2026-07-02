package unpack_test

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/unpack"
)

func BenchmarkSandboxExtraction(b *testing.B) {
	if _, err := exec.LookPath("unrar"); err != nil {
		b.Skip("unrar binary not found in PATH")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		b.Skip("bwrap binary not found in PATH")
	}

	rarPath := filepath.Join("testdata", "single_rar5.rar")
	if _, err := os.Stat(rarPath); err != nil {
		b.Skipf("test archive %s not present", rarPath)
	}

	archive := unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "single_rar5",
		MainFile: rarPath,
		Parts:    []string{rarPath},
	}

	silentLog := slog.New(slog.DiscardHandler)

	b.Run("Direct", func(b *testing.B) {
		outDir := b.TempDir()
		opts := unpack.Options{
			UseGoRAR:       false,
			OverwriteFiles: true,
			Sandbox: cmdutil.SandboxConfig{
				Enabled: false,
			},
		}
		b.ResetTimer()
		for range b.N {
			res, err := unpack.UnRAR(b.Context(), silentLog, archive, outDir, "", opts)
			if err != nil || res.ExitCode != 0 {
				b.Fatalf("direct extraction failed: %v, output: %s", err, res.Output)
			}
		}
	})

	b.Run("Sandboxed", func(b *testing.B) {
		outDir := b.TempDir()
		opts := unpack.Options{
			UseGoRAR:       false,
			OverwriteFiles: true,
			Sandbox: cmdutil.SandboxConfig{
				Enabled:   true,
				Strict:    true,
				TargetDir: outDir,
			},
		}
		b.ResetTimer()
		for range b.N {
			res, err := unpack.UnRAR(b.Context(), silentLog, archive, outDir, "", opts)
			if err != nil || res.ExitCode != 0 {
				b.Fatalf("sandboxed extraction failed: %v, output: %s", err, res.Output)
			}
		}
	})
}
