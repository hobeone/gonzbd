package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/par2"
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

// par2FileSpec describes one file for buildPar2Job: its subject, assembled
// CRC32 (set post-construction via SetFileCRC32), and byte count.
type par2FileSpec struct {
	subject string
	crc     uint32
	bytes   int64
}

// newPar2Job builds a real, not-yet-added *queue.Job via queue.NewJob (with
// OnDemandPar2 enabled, so any *.volNNN+MM.par2 subject classifies and
// defers correctly). Callers needing a custom ID must set job.ID before
// adding it to a queue — Queue.Add indexes by whatever ID is set at Add
// time, so overriding it afterward silently orphans the original key.
func newPar2Job(t *testing.T, specs []par2FileSpec) *queue.Job {
	t.Helper()
	parsed := &nzb.NZB{}
	for i, f := range specs {
		b := f.bytes
		if b == 0 {
			b = 1
		}
		parsed.Files = append(parsed.Files, nzb.File{
			Subject:  f.subject,
			Bytes:    b,
			Articles: []nzb.Article{{ID: fmt.Sprintf("f%d@t", i), Bytes: int(b), Number: 1}},
		})
	}
	qjob, err := queue.NewJob(parsed, queue.AddOptions{Filename: "t.nzb", OnDemandPar2: true}, fsutil.SanitizeOptions{})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	return qjob
}

// buildPar2Job builds a job via newPar2Job, adds it to a fresh queue, and
// sets each file's assembled CRC32 through the queue's real mutator.
func buildPar2Job(t *testing.T, specs []par2FileSpec) (*queue.Queue, *queue.Job) {
	t.Helper()
	qjob := newPar2Job(t, specs)
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i, f := range specs {
		if f.crc == 0 {
			continue
		}
		if err := q.SetFileCRC32(qjob.ID, i, f.crc); err != nil {
			t.Fatalf("SetFileCRC32: %v", err)
		}
	}
	return q, qjob
}

func TestPar2NeedsRecovery(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	deferredVol := par2FileSpec{subject: "data.vol000+01.par2", bytes: 1}

	t.Run("clean data verifies and skips recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		_, qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", crc: 0x1068AFA6, bytes: 100},
			deferredVol,
		})
		if got, _ := par2NeedsRecovery(dir, mustManifest(t, qjob), qjob.Progress(), log, par2.DefaultParseOptions()); got {
			t.Error("clean download must NOT trigger recovery-volume download")
		}
	})

	t.Run("CRC mismatch triggers recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		_, qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", crc: 0xDEADBEEF, bytes: 100},
			deferredVol,
		})
		if got, reason := par2NeedsRecovery(dir, mustManifest(t, qjob), qjob.Progress(), log, par2.DefaultParseOptions()); !got {
			t.Error("corrupt file (CRC mismatch) must trigger recovery")
		} else if !strings.Contains(reason, "corruption/CRC mismatch") {
			t.Errorf("expected CRC mismatch reason, got: %q", reason)
		}
	})

	t.Run("failed download (no assembled CRC) triggers recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		_, qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", bytes: 100},
			deferredVol,
		})
		if got, reason := par2NeedsRecovery(dir, mustManifest(t, qjob), qjob.Progress(), log, par2.DefaultParseOptions()); !got {
			t.Error("par2-tracked file with no CRC must trigger recovery")
		} else if !strings.Contains(reason, "failed download") {
			t.Errorf("expected failed download reason, got: %q", reason)
		}
	})

	t.Run("missing par2 index falls back to fetching recovery", func(t *testing.T) {
		dir := t.TempDir() // empty — no par2 index on disk
		_, qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", crc: 0x1068AFA6, bytes: 100},
		})
		if got, reason := par2NeedsRecovery(dir, mustManifest(t, qjob), qjob.Progress(), log, par2.DefaultParseOptions()); !got {
			t.Error("no usable par2 index must fall back to fetching recovery volumes")
		} else if !strings.Contains(reason, "no usable par2 index found") {
			t.Errorf("expected missing index reason, got: %q", reason)
		}
	})

	t.Run("non-matching par2 set protects different files (Layout B) skips recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir) // protects data.bin
		_, qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "other.bin", crc: 0x99999999, bytes: 100},
			deferredVol,
		})
		if got, _ := par2NeedsRecovery(dir, mustManifest(t, qjob), qjob.Progress(), log, par2.DefaultParseOptions()); got {
			t.Error("par2 protecting different files must NOT trigger recovery-volume download")
		}
	})
}

func TestMaybeReleaseRecoveryVolumes(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) {
		c.General.DownloadDir = dir
	})

	const jobID = "job-1"
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "job-name"
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := q.SetFileCRC32(jobID, 0, 0x1068AFA6); err != nil {
		t.Fatalf("SetFileCRC32: %v", err)
	}

	app := &Application{
		queue:   q,
		log:     log,
		config:  cfg,
		emitter: dummyEmitter{},
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
		m := mustManifest(t, snapAfter)
		for fi := range m.NumFiles() {
			if m.FileIsPar2Recovery(fi) {
				t.Error("deferred par2 recovery files were not discarded from the job")
			}
		}
	})

	t.Run("corrupt data undeferes recovery volumes", func(t *testing.T) {
		// Create a new job with mismatched CRC.
		const jobCorruptID = "job-corrupt"
		jobCorrupt := newPar2Job(t, []par2FileSpec{
			{subject: "data.bin", bytes: 100},
			{subject: "data.vol000+01.par2", bytes: 100},
		})
		jobCorrupt.ID = jobCorruptID
		jobCorrupt.Name = "job-corrupt-name"
		if err := q.Add(jobCorrupt); err != nil {
			t.Fatal(err)
		}
		if err := q.SetFileCRC32(jobCorruptID, 0, 0xDEADBEEF); err != nil {
			t.Fatal(err)
		}

		dirCorrupt := filepath.Join(dir, "job-corrupt-name")
		if err := os.MkdirAll(dirCorrupt, 0o750); err != nil {
			t.Fatal(err)
		}
		copyFixturePar2(t, dirCorrupt)

		snap := q.SnapshotJob(jobCorruptID)
		if !app.maybeReleaseRecoveryVolumes(t.Context(), jobCorruptID, snap) {
			t.Error("maybeReleaseRecoveryVolumes must return true when verification fails")
		}

		// Verify that the deferred recovery volume was undeferred.
		snapAfter := q.SnapshotJob(jobCorruptID)
		m, p := mustManifest(t, snapAfter), snapAfter.Progress()
		found := false
		for fi := range m.NumFiles() {
			if m.FileIsPar2Recovery(fi) {
				found = true
				if p.FileDeferred(fi) {
					t.Error("deferred recovery volume was not undeferred")
				}
			}
		}
		if !found {
			t.Error("recovery volume disappeared from job")
		}
	})
}
