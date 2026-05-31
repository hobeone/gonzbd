package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/queue"
)

// copyFixturePar2 copies the shared par2 index fixture (which protects
// data.bin with CRC32 0x1068AFA6) into dir, so par2NeedsRecovery can scan it.
func copyFixturePar2(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile("../../test/fixtures/par2/data.par2")
	if err != nil {
		t.Skipf("par2 fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.par2"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPar2NeedsRecovery(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A realistic file set: the protected content file plus a deferred
	// recovery volume (which par2 does not list, so it must not affect the
	// verdict).
	deferredVol := queue.JobFile{Subject: "data.vol000+01.par2", IsPar2Recovery: true, Deferred: true}

	t.Run("clean data verifies and skips recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		files := []queue.JobFile{
			{Subject: "data.bin", AssembledCRC32: 0x1068AFA6, Bytes: 100},
			deferredVol,
		}
		if par2NeedsRecovery(dir, files, log) {
			t.Error("clean download must NOT trigger recovery-volume download")
		}
	})

	t.Run("CRC mismatch triggers recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		files := []queue.JobFile{{Subject: "data.bin", AssembledCRC32: 0xDEADBEEF}, deferredVol}
		if !par2NeedsRecovery(dir, files, log) {
			t.Error("corrupt file (CRC mismatch) must trigger recovery")
		}
	})

	t.Run("failed download (no assembled CRC) triggers recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		files := []queue.JobFile{{Subject: "data.bin", AssembledCRC32: 0}, deferredVol}
		if !par2NeedsRecovery(dir, files, log) {
			t.Error("par2-tracked file with no CRC must trigger recovery")
		}
	})

	t.Run("missing par2 index falls back to fetching recovery", func(t *testing.T) {
		dir := t.TempDir() // empty — no par2 index on disk
		files := []queue.JobFile{{Subject: "data.bin", AssembledCRC32: 0x1068AFA6}}
		if !par2NeedsRecovery(dir, files, log) {
			t.Error("no usable par2 index must fall back to fetching recovery volumes")
		}
	})
}
