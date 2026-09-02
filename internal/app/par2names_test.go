package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/queue"
)

// TestApplyPar2Names_RenamesAndRecordsTheNewName pins the owner claim in
// applyPar2Names' doc: the on-disk move and the resolved-name update are one
// operation.
//
// Half of this passing is not enough. A version that renames the file but
// never calls SetFileFilename leaves JobProgress.Filename describing a path
// that no longer exists, and every later reader — quickcheck's VerifyCRCs
// among them — then matches against a stale name. So both halves are asserted,
// and the mutation spec neuters each independently.
func TestApplyPar2Names_RenamesAndRecordsTheNewName(t *testing.T) {
	t.Parallel()

	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "rename-job"
	const obfuscated = "7xq6N6P340dCh9Lnih5hY3jsArfSN1"

	qjob := newPar2Job(t, []par2FileSpec{
		{subject: obfuscated, bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "rename-job-name"
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	jobDir := filepath.Join(downloadDir, qjob.Name)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir) // protects data.bin
	copyFixturePayload(t, jobDir, obfuscated)

	app := &Application{queue: q, log: slog.New(slog.DiscardHandler), config: cfg, emitter: dummyEmitter{}}

	sets, err := par2.FindPar2Files(jobDir, par2.DefaultParseOptions())
	if err != nil || len(sets) == 0 {
		t.Fatalf("FindPar2Files: %v (%d sets)", err, len(sets))
	}

	snap := q.SnapshotJob(jobID)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	applied := app.applyPar2Names(jobID, jobDir, sets, m, snap.Progress(), app.log, par2.DefaultParseOptions())
	if applied != 1 {
		t.Fatalf("applyPar2Names recorded %d renames, want 1", applied)
	}

	// Half one: the file moved.
	if _, err := os.Stat(filepath.Join(jobDir, "data.bin")); err != nil {
		t.Errorf("data.bin is not on disk after the rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jobDir, obfuscated)); !os.IsNotExist(err) {
		t.Errorf("the obfuscated name still exists; stat returned %v", err)
	}

	// Half two: the queue knows where it went. Read from a fresh snapshot,
	// since that is how every real consumer sees it.
	after := q.SnapshotJob(jobID)
	if got := after.Progress().FileFilename(0); got != "data.bin" {
		t.Errorf("resolved filename = %q, want data.bin — the file moved but nothing recorded it", got)
	}
}

// TestApplyPar2Names_FindsAFileByItsRecordedName covers the case the test
// above cannot: a file already carrying a resolved name that differs from its
// NZB subject.
//
// This is the ordinary production state, not an edge case. pipeline.go's
// registerFile records the name the assembler actually wrote — for an
// obfuscated post that is the yEnc name, which has no relation to the subject.
// So by the time applyPar2Names runs, looking a file up by its subject alone
// finds nothing and the rename goes unrecorded.
//
// The first test cannot see this: with no resolved name recorded, the subject
// IS the resolved name, so both lookups agree and a subject-only index passes.
func TestApplyPar2Names_FindsAFileByItsRecordedName(t *testing.T) {
	t.Parallel()

	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "recorded-name-job"
	const subject = "[1_2] - _abcd…_ yEnc (1_1)" // what the NZB said
	const yencName = "7xq6N6P340dCh9Lnih5hY3jsArfSN1"

	qjob := newPar2Job(t, []par2FileSpec{
		{subject: subject, bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "recorded-name-job-name"
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// What registerFile does at first write.
	if err := q.SetFileFilename(jobID, 0, yencName); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}

	jobDir := filepath.Join(downloadDir, qjob.Name)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, yencName)

	app := &Application{queue: q, log: slog.New(slog.DiscardHandler), config: cfg, emitter: dummyEmitter{}}

	sets, err := par2.FindPar2Files(jobDir, par2.DefaultParseOptions())
	if err != nil || len(sets) == 0 {
		t.Fatalf("FindPar2Files: %v (%d sets)", err, len(sets))
	}
	snap := q.SnapshotJob(jobID)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	// Fixture guard: if these ever coincide the test degenerates into the one
	// above and stops covering anything.
	if m.FileSubject(0) == yencName {
		t.Fatal("fixture is wrong: the subject and the recorded name must differ")
	}

	if applied := app.applyPar2Names(jobID, jobDir, sets, m, snap.Progress(), app.log, par2.DefaultParseOptions()); applied != 1 {
		t.Fatalf("applyPar2Names recorded %d renames, want 1", applied)
	}

	after := q.SnapshotJob(jobID)
	if got := after.Progress().FileFilename(0); got != "data.bin" {
		t.Errorf("resolved filename = %q, want data.bin — the file was looked up by subject rather than by the name it actually had", got)
	}
}
