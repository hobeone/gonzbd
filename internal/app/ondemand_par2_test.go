package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
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
		if got, _ := par2NeedsRecovery(dir, files, log); got {
			t.Error("clean download must NOT trigger recovery-volume download")
		}
	})

	t.Run("CRC mismatch triggers recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		files := []queue.JobFile{{Subject: "data.bin", AssembledCRC32: 0xDEADBEEF}, deferredVol}
		if got, reason := par2NeedsRecovery(dir, files, log); !got {
			t.Error("corrupt file (CRC mismatch) must trigger recovery")
		} else if !strings.Contains(reason, "corruption/CRC mismatch") {
			t.Errorf("expected CRC mismatch reason, got: %q", reason)
		}
	})

	t.Run("failed download (no assembled CRC) triggers recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		files := []queue.JobFile{{Subject: "data.bin", AssembledCRC32: 0}, deferredVol}
		if got, reason := par2NeedsRecovery(dir, files, log); !got {
			t.Error("par2-tracked file with no CRC must trigger recovery")
		} else if !strings.Contains(reason, "failed download") {
			t.Errorf("expected failed download reason, got: %q", reason)
		}
	})

	t.Run("missing par2 index falls back to fetching recovery", func(t *testing.T) {
		dir := t.TempDir() // empty — no par2 index on disk
		files := []queue.JobFile{{Subject: "data.bin", AssembledCRC32: 0x1068AFA6}}
		if got, reason := par2NeedsRecovery(dir, files, log); !got {
			t.Error("no usable par2 index must fall back to fetching recovery volumes")
		} else if !strings.Contains(reason, "no usable par2 index found") {
			t.Errorf("expected missing index reason, got: %q", reason)
		}
	})

	t.Run("non-matching par2 set protects different files (Layout B) skips recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir) // protects data.bin
		files := []queue.JobFile{
			{Subject: "other.bin", AssembledCRC32: 0x99999999, Bytes: 100},
			deferredVol,
		}
		if got, _ := par2NeedsRecovery(dir, files, log); got {
			t.Error("par2 protecting different files must NOT trigger recovery-volume download")
		}
	})
}

func TestMaybeReleaseRecoveryVolumes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()

	q := queue.New()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.DownloadDir = dir
	})
	app := &Application{
		queue:  q,
		log:    log,
		config: cfg,
	}

	const jobID = "job-1"
	job := &queue.Job{
		ID:   jobID,
		Name: "job-name",
		Files: []queue.JobFile{
			{Subject: "data.bin", AssembledCRC32: 0x1068AFA6, Bytes: 100},
			{Subject: "data.vol000+01.par2", IsPar2Recovery: true, Deferred: true, Bytes: 100},
		},
	}
	if err := q.Add(job); err != nil {
		t.Fatal(err)
	}

	t.Run("context cancelled returns false", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(t.Context())
		cancel()

		snap := q.SnapshotJob(jobID)
		if app.maybeReleaseRecoveryVolumes(cancelledCtx, jobID, snap) {
			t.Error("maybeReleaseRecoveryVolumes must return false when context is cancelled")
		}
	})

	t.Run("clean verification discards deferred par2", func(t *testing.T) {
		dirClean := filepath.Join(dir, "job-name")
		if err := os.MkdirAll(dirClean, 0o750); err != nil {
			t.Fatal(err)
		}
		copyFixturePar2(t, dirClean)

		snap := q.SnapshotJob(jobID)
		if app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
			t.Error("maybeReleaseRecoveryVolumes must return false when verification is clean")
		}

		// Verify that the deferred PAR2 files are removed from the queue.
		snapAfter := q.SnapshotJob(jobID)
		for _, f := range snapAfter.Files {
			if f.IsPar2Recovery {
				t.Error("deferred par2 recovery files were not discarded from the job")
			}
		}
	})

	t.Run("corrupt data undeferes recovery volumes", func(t *testing.T) {
		// Create a new job with mismatched CRC.
		const jobCorruptID = "job-corrupt"
		jobCorrupt := &queue.Job{
			ID:   jobCorruptID,
			Name: "job-corrupt-name",
			Files: []queue.JobFile{
				{Subject: "data.bin", AssembledCRC32: 0xDEADBEEF, Bytes: 100},
				{Subject: "data.vol000+01.par2", IsPar2Recovery: true, Deferred: true, Bytes: 100},
			},
		}
		if err := q.Add(jobCorrupt); err != nil {
			t.Fatal(err)
		}

		dirCorrupt := filepath.Join(dir, "job-corrupt-name")
		if err := os.MkdirAll(dirCorrupt, 0o750); err != nil {
			t.Fatal(err)
		}
		copyFixturePar2(t, dirCorrupt)

		snap := q.SnapshotJob(jobCorruptID)
		app.emitter = dummyEmitter{}
		if !app.maybeReleaseRecoveryVolumes(t.Context(), jobCorruptID, snap) {
			t.Error("maybeReleaseRecoveryVolumes must return true when verification fails")
		}

		// Verify that the deferred recovery volume was undeferred.
		snapAfter := q.SnapshotJob(jobCorruptID)
		found := false
		for _, f := range snapAfter.Files {
			if f.IsPar2Recovery {
				found = true
				if f.Deferred {
					t.Error("deferred recovery volume was not undeferred")
				}
			}
		}
		if !found {
			t.Error("recovery volume disappeared from job")
		}
	})
}
