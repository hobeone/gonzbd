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
	"github.com/hobeone/gonzbd/internal/durability"
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

// copyFixturePayload puts the protected file itself on disk beside the index.
//
// par2NeedsRecovery now IDENTIFIES the delivered files before verifying them,
// and identification reads the directory — an index describing data.bin with
// no data.bin present means the file is genuinely missing, and the honest
// answer is to fetch the recovery volumes. Verification alone never looked at
// the filesystem, which is why these fixtures did not need it before.
func copyFixturePayload(t *testing.T, dir, asName string) {
	t.Helper()
	b, err := os.ReadFile("../../test/fixtures/par2/data.bin")
	if err != nil {
		t.Skipf("par2 payload fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, asName), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedFileCRC gives one of a job's files an assembled CRC32.
//
// It goes through Queue.SetFileCRC32FromRuns rather than writing the field,
// which means presenting the record that would have earned the value: one
// durable run at offset 0 spanning every article of the file. That is the
// gatekeeper's point — the CRC and its evidence arrive together — and it keeps
// these fixtures describing a state the program can actually reach.
func seedFileCRC(t *testing.T, q *queue.Queue, job *queue.Job, fileIdx int, crc uint32) {
	t.Helper()
	m, err := job.Manifest()
	if err != nil {
		t.Fatalf("manifest for the CRC fixture: %v", err)
	}
	lo, hi := m.FileRange(fileIdx)
	runs := []durability.Run{{
		FileIdx:     int32(fileIdx),
		FirstArtIdx: int32(lo),
		LastArtIdx:  int32(hi - 1),
		Offset:      0,
		Length:      m.FileBytes(fileIdx),
		CRC32:       crc,
	}}
	if err := q.SetFileCRC32FromRuns(job.ID, fileIdx, runs); err != nil {
		t.Fatalf("SetFileCRC32FromRuns(%d): %v", fileIdx, err)
	}
}

// par2FileSpec describes one file for buildPar2Job: its subject, assembled
// CRC32 (seeded post-construction via seedFileCRC), and byte count.
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
		seedFileCRC(t, q, qjob, i, f.crc)
	}
	return q, qjob
}

func TestPar2NeedsRecovery(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	deferredVol := par2FileSpec{subject: "data.vol000+01.par2", bytes: 1}

	t.Run("clean data verifies and skips recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		copyFixturePayload(t, dir, "data.bin")
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
		copyFixturePayload(t, dir, "data.bin")
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
		copyFixturePayload(t, dir, "data.bin")
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

	// This subtest used to assert the opposite, and the reversal is the point
	// of the change rather than a side effect of it.
	//
	// A par2 set describing a file this download does not contain is
	// indistinguishable, from here, between three causes: the poster's par2
	// protects the extracted contents rather than the archives, the file
	// failed to download, or a name defeated identification. The old code
	// read this state as proof of the first and DISCARDED the recovery
	// volumes — a terminal decision, since nothing downstream can fetch them
	// (post-processing's NeedRequeue has no consumer).
	//
	// Fetching costs bandwidth on a job that may not need it. Discarding
	// costs an unrepairable job that did. At a decision that cannot be
	// revisited, the recoverable branch is the correct one.
	t.Run("a par2 entry matching nothing delivered fetches recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir) // protects data.bin
		// Genuinely different content, not the fixture payload under another
		// name: identification is by CONTENT, so copying data.bin to
		// other.bin would be correctly identified AS data.bin and this
		// subtest would assert the opposite of what it means to.
		if err := os.WriteFile(filepath.Join(dir, "other.bin"), []byte(strings.Repeat("not-the-protected-file", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "other.bin", crc: 0x99999999, bytes: 100},
			deferredVol,
		})
		got, reason := par2NeedsRecovery(dir, mustManifest(t, qjob), qjob.Progress(), log, par2.DefaultParseOptions())
		if !got {
			t.Fatal("a par2 set describing a file we do not have must fetch the recovery volumes, not discard them")
		}
		if !strings.Contains(reason, "not found in this download") {
			t.Errorf("expected an unaccounted-file reason, got: %q", reason)
		}
	})

	// The counterpart, and what stops the branch above from being a blanket
	// "always fetch": an obfuscated file is still the file par2 describes, and
	// content identification finds it. This is the case measured on a real
	// release (#492), where the shipped code discarded the volumes.
	t.Run("an obfuscated file is identified by content, not fetched blindly", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir) // protects data.bin
		copyFixturePayload(t, dir, "7xq6N6P340dCh9Lnih5hY3jsArfSN1")

		// Stops at identification deliberately. Verification still matches by
		// name, and the rename that makes those names line up is applied by
		// applyPar2Names at the caller, not inside par2NeedsRecovery — a
		// function named for a question does not move files. What this pins
		// is the app-level claim against the REAL fixture: the obfuscated
		// file is accounted for, so the branch that discards volumes is not
		// reached.
		sets, err := par2.FindPar2Files(dir, par2.DefaultParseOptions())
		if err != nil || len(sets) == 0 {
			t.Fatalf("FindPar2Files: %v (%d sets)", err, len(sets))
		}
		id, err := par2.Identify(dir, sets, log)
		if err != nil {
			t.Fatalf("Identify: %v", err)
		}
		if !id.Accounted() {
			t.Fatalf("obfuscated file left unaccounted: %+v", id.Unaccounted)
		}
		if len(id.Files) != 1 || id.Files[0].Desc.FileName != "data.bin" {
			t.Fatalf("identified %+v, want the one entry data.bin", id.Files)
		}
		if !id.Files[0].NeedsRename() {
			t.Error("NeedsRename() = false for an obfuscated file")
		}
	})
}

func TestMaybeReleaseRecoveryVolumes(t *testing.T) {
	t.Parallel()
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
	seedFileCRC(t, q, qjob, 0, 0x1068AFA6)

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
		copyFixturePayload(t, dirClean, "data.bin")

		snap := q.SnapshotJob(jobID)
		if app.maybeReleaseRecoveryVolumes(t.Context(), jobID, snap) {
			t.Error("maybeReleaseRecoveryVolumes must return false when verification is clean")
		}

		// Verify that the recovery volume is marked FetchNever rather than
		// removed from the job: DiscardDeferredPar2 no longer changes the
		// file set (see its doc comment in internal/queue/queue.go), so the
		// recovery volume is still in the manifest, just no longer awaiting
		// the CRC verdict.
		snapAfter := q.SnapshotJob(jobID)
		m := mustManifest(t, snapAfter)
		sawRecovery := false
		for fi := range m.NumFiles() {
			if !m.FileIsPar2Recovery(fi) {
				continue
			}
			sawRecovery = true
			if got := snapAfter.Progress().FileFetchPolicy(fi); got != queue.FetchNever {
				t.Errorf("recovery file %d policy = %d, want FetchNever after a clean verification discarded it", fi, got)
			}
		}
		if !sawRecovery {
			t.Fatal("fixture guard: expected a par2 recovery file to still be present in the manifest")
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
		seedFileCRC(t, q, jobCorrupt, 0, 0xDEADBEEF)

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
				if p.FileFetchPolicy(fi) != queue.FetchAlways {
					t.Error("deferred recovery volume was not undeferred")
				}
			}
		}
		if !found {
			t.Error("recovery volume disappeared from job")
		}
	})
}
