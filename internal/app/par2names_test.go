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

// TestApplyPar2Names_SubdirectoryRelocationIsNotRecorded drives a real
// subdirectory relocation through applyPar2Names.
//
// The file must move, and the move must NOT be written to
// JobProgress.Filename: that field goes back through fsutil.JoinSafe on a
// resume, and SanitizeFilename would turn "Screens/data.bin" into
// "Screens_data.bin", sending a retry to write a file that is not there.
func TestApplyPar2Names_SubdirectoryRelocationIsNotRecorded(t *testing.T) {
	t.Parallel()

	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "subdir-job"
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "subdir-job-name"
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	jobDir := filepath.Join(downloadDir, qjob.Name)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// A set whose only entry is "Screens/data.bin".
	b, err := os.ReadFile("../../test/fixtures/par2/subdir.par2")
	if err != nil {
		t.Skipf("subdir par2 fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "subdir.par2"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	copyFixturePayload(t, jobDir, "data.bin")

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

	if applied := app.applyPar2Names(jobID, jobDir, sets, m, snap.Progress(), app.log, par2.DefaultParseOptions()); applied != 0 {
		t.Errorf("recorded %d renames; a subdirectory path cannot be stored as a resolved name", applied)
	}

	// The relocation itself still happened.
	if _, err := os.Stat(filepath.Join(jobDir, "Screens", "data.bin")); err != nil {
		t.Fatalf("fixture guard: the file was not relocated, so this proves nothing: %v", err)
	}
	if got := q.SnapshotJob(jobID).Progress().FileFilename(0); got != "" {
		t.Errorf("resolved filename = %q; a path here resolves to the wrong file on resume", got)
	}
}

// TestRecordableAsResolvedName pins what JobProgress.Filename can hold.
//
// The field is fed back through fsutil.JoinSafe on a resume, and
// SanitizeFilename rewrites "/" and "\" to "_" — so a path recorded there
// resolves to a different file than the one on disk. A flat rename, which is
// the deobfuscation case this path exists for, is fine.
func TestRecordableAsResolvedName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		to   string
		want bool
	}{
		{"data.bin", true},
		{"movie.part1.rar", true},
		{"Screens/shot.jpg", false},
		{`Screens\shot.jpg`, false},
		{"a/b/c.txt", false},
	} {
		if got := recordableAsResolvedName(tc.to); got != tc.want {
			t.Errorf("recordableAsResolvedName(%q) = %v, want %v", tc.to, got, tc.want)
		}
	}
}

// TestResolvedName pins the precedence both par2 call sites already use: the
// recorded on-disk name wins over the NZB subject, and the subject is the
// fallback until one is recorded.
//
// The precedence is what makes applyPar2Names find a file that the assembler
// wrote under its yEnc name rather than its subject, which is the ordinary
// obfuscated case.
func TestResolvedName(t *testing.T) {
	t.Parallel()

	const jobID = "resolved-name-job"
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "subject-name.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	snap := q.SnapshotJob(jobID)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	if got := resolvedName(m, snap.Progress(), 0); got != "subject-name.bin" {
		t.Errorf("with nothing recorded, resolvedName = %q, want the subject", got)
	}

	if err := q.SetFileFilename(jobID, 0, "actual-on-disk.bin"); err != nil {
		t.Fatalf("SetFileFilename: %v", err)
	}
	after := q.SnapshotJob(jobID)
	if got := resolvedName(m, after.Progress(), 0); got != "actual-on-disk.bin" {
		t.Errorf("with a name recorded, resolvedName = %q, want the recorded name", got)
	}
}

// TestApplyPar2Names_NothingToRename covers the ordinary job: every file is
// already at the path par2 records, so there is no rename to make and nothing
// to record. This is the common path, and it must not touch the queue.
func TestApplyPar2Names_NothingToRename(t *testing.T) {
	t.Parallel()

	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "no-rename-job"
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "no-rename-job-name"
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	jobDir := filepath.Join(downloadDir, qjob.Name)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin") // already correctly named

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

	if applied := app.applyPar2Names(jobID, jobDir, sets, m, snap.Progress(), app.log, par2.DefaultParseOptions()); applied != 0 {
		t.Errorf("applyPar2Names recorded %d renames for a correctly-named job, want 0", applied)
	}
	if _, err := os.Stat(filepath.Join(jobDir, "data.bin")); err != nil {
		t.Errorf("data.bin moved: %v", err)
	}
	if got := q.SnapshotJob(jobID).Progress().FileFilename(0); got != "" {
		t.Errorf("resolved filename = %q; nothing moved, so nothing should have been recorded", got)
	}
}

// TestApplyPar2Names_RenameOfAFileTheManifestDoesNotName covers the branch
// where par2 relocates something with no queue row to update — an extracted
// file, or a name a previous run already corrected. The move is correct and
// there is simply nothing to record, so it must not be counted or fail.
func TestApplyPar2Names_RenameOfAFileTheManifestDoesNotName(t *testing.T) {
	t.Parallel()

	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "stranger-job"
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "something-else.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "stranger-job-name"
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	jobDir := filepath.Join(downloadDir, qjob.Name)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixturePar2(t, jobDir)
	// On disk under a name the manifest never mentions, so identification
	// matches it by content and renames it, but no row corresponds.
	copyFixturePayload(t, jobDir, "stranger-on-disk")

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

	if applied := app.applyPar2Names(jobID, jobDir, sets, m, snap.Progress(), app.log, par2.DefaultParseOptions()); applied != 0 {
		t.Errorf("recorded %d renames, want 0 — no manifest row names that file", applied)
	}
	// The relocation still happened; only the bookkeeping had no target.
	if _, err := os.Stat(filepath.Join(jobDir, "data.bin")); err != nil {
		t.Errorf("the file was not relocated on disk: %v", err)
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
