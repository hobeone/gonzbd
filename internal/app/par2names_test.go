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
// recordPar2Names' doc: the on-disk move and the resolved-name update are one
// operation.
//
// Half of this passing is not enough. A version that renames the file but
// never calls SetFileFilename leaves JobProgress.Filename describing a path
// that no longer exists, and every later reader — quickcheck's verification
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

	applied := app.recordPar2Names(jobID, jobDir, mustAssess(t, jobDir, sets, app.log), m, snap.Progress(), app.log)
	if applied != 1 {
		t.Fatalf("recordPar2Names recorded %d renames, want 1", applied)
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

// TestApplyPar2Names_SubdirectoryRelocationRecordsTheBasename drives a real
// subdirectory relocation through recordPar2Names.
//
// The file must move, and what gets written to JobProgress.Filename must be
// the BASENAME — neither the full path nor nothing at all. The path cannot go
// there: the field goes back through fsutil.JoinSafe on a resume, and
// SanitizeFilename would turn "Screens/data.bin" into "Screens_data.bin",
// sending a retry to write a file that is not there. Recording nothing, which
// this test previously asserted, leaves the pre-rename name standing — equally
// absent from disk, and matched against no par2 entry by verification, which
// indexes by basename.
func TestApplyPar2Names_SubdirectoryRelocationRecordsTheBasename(t *testing.T) {
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
	copyFixtureSubdirPar2(t, jobDir)
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

	if applied := app.recordPar2Names(jobID, jobDir, mustAssess(t, jobDir, sets, app.log), m, snap.Progress(), app.log); applied != 1 {
		t.Errorf("recorded %d renames, want 1: a subdirectory relocation must still record SOMETHING, or every later "+
			"reader keeps matching against a name that no longer exists on disk", applied)
	}

	// The relocation itself still happened.
	if _, err := os.Stat(filepath.Join(jobDir, "Screens", "data.bin")); err != nil {
		t.Fatalf("fixture guard: the file was not relocated, so this proves nothing: %v", err)
	}

	// The basename, not the path and not the stale original.
	//
	// The path cannot be stored: registerFile feeds this field back through
	// fsutil.JoinSafe on a resume and SanitizeFilename rewrites "/" to "_",
	// so "Screens/data.bin" would resolve to "Screens_data.bin".
	//
	// Storing nothing was the previous behaviour and was worse than it looked:
	// the field then kept the pre-rename name, which names nothing on disk
	// either, and par2.Assess — which joins on the
	// basename — matched it against no entry and counted the file Unverified,
	// marking an intact job damaged.
	if got := q.SnapshotJob(jobID).Progress().FileFilename(0); got != "data.bin" {
		t.Errorf("resolved filename = %q, want %q: the basename is what par2.Assess joins on, so it is what "+
			"lets a relocated file still be matched to its entry", got, "data.bin")
	}
}

// TestApplyPar2Names_IsIdempotent pins that a second pass over an
// already-relocated directory is a no-op rather than a fresh finding.
//
// This is not a hypothetical tidiness property. internal/app runs
// recordPar2Names and then par2Verdict back to back over the same
// directory, and par2Verdict identifies again from scratch. Identify
// reads one directory level and skips directories, so without the pass that
// checks whether an entry's par2 path already exists on disk, the second look
// cannot see the file the first one just moved into "Screens/" — it reports
// the entry unaccounted, and a healthy job with any subdirectory in its par2
// set fetches its entire recovery set.
func TestApplyPar2Names_IsIdempotent(t *testing.T) {
	t.Parallel()

	downloadDir := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.With(func(c *config.Config) { c.General.DownloadDir = downloadDir })

	const jobID = "idempotent-job"
	qjob := newPar2Job(t, []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	qjob.ID = jobID
	qjob.Name = "idempotent-job-name"
	q := queue.New()
	if err := q.Add(qjob); err != nil {
		t.Fatalf("Add: %v", err)
	}

	jobDir := filepath.Join(downloadDir, qjob.Name)
	if err := os.MkdirAll(jobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	copyFixtureSubdirPar2(t, jobDir)
	copyFixturePayload(t, jobDir, "data.bin")

	app := &Application{queue: q, log: slog.New(slog.DiscardHandler), config: cfg, emitter: dummyEmitter{}}
	sets, err := par2.FindPar2Files(jobDir, par2.DefaultParseOptions())
	if err != nil || len(sets) == 0 {
		t.Fatalf("FindPar2Files: %v (%d sets)", err, len(sets))
	}

	run := func() int {
		snap := q.SnapshotJob(jobID)
		m, mErr := snap.Manifest()
		if mErr != nil {
			t.Fatalf("Manifest: %v", mErr)
		}
		return app.recordPar2Names(jobID, jobDir, mustAssess(t, jobDir, sets, app.log), m, snap.Progress(), app.log)
	}

	if first := run(); first != 1 {
		t.Fatalf("fixture guard: first pass recorded %d renames, want 1; nothing was relocated so the second pass proves nothing", first)
	}
	if second := run(); second != 0 {
		t.Errorf("second pass recorded %d renames over an already-relocated directory, want 0", second)
	}

	// And the entry is accounted for on the second look, which is the
	// property par2Verdict actually consumes.
	id, err := par2.IdentifyWithOptions(jobDir, sets, app.log, par2.DefaultParseOptions())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if !id.Accounted() {
		t.Errorf("%d par2 entr(y/ies) unaccounted after relocation; the file is on disk at the path par2 names, "+
			"so reporting it missing fetches a recovery set the job does not need", len(id.Unaccounted))
	}
}

// TestResolvedNameFor pins what JobProgress.Filename can hold, and what a
// rename target is reduced to so that it can.
//
// The field is fed back through fsutil.JoinSafe on a resume, and
// SanitizeFilename rewrites "/" and "\" to "_" — so a path recorded there
// resolves to a different file than the one on disk. A flat rename, which is
// the deobfuscation case this path exists for, passes through untouched.
//
// A subdirectory target is reduced to its basename rather than dropped. That
// is what par2.Assess joins on (verifyIdentified keys
// "basename → entry"), so the recorded name still matches the entry the file
// was relocated to satisfy; dropping it left the stale obfuscated name in
// place, which matched nothing and marked a healthy job damaged.
func TestResolvedNameFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		to   string
		want string
	}{
		{"data.bin", "data.bin"},
		{"movie.part1.rar", "movie.part1.rar"},
		{"Screens/shot.jpg", "shot.jpg"},
		{`Screens\shot.jpg`, `Screens\shot.jpg`},
		{"a/b/c.txt", "c.txt"},
	} {
		if got := resolvedNameFor(tc.to); got != tc.want {
			t.Errorf("resolvedNameFor(%q) = %q, want %q", tc.to, got, tc.want)
		}
	}
}

// TestResolvedName pins the precedence both par2 call sites already use: the
// recorded on-disk name wins over the NZB subject, and the subject is the
// fallback until one is recorded.
//
// The precedence is what makes recordPar2Names find a file that the assembler
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

	if applied := app.recordPar2Names(jobID, jobDir, mustAssess(t, jobDir, sets, app.log), m, snap.Progress(), app.log); applied != 0 {
		t.Errorf("recordPar2Names recorded %d renames for a correctly-named job, want 0", applied)
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

	if applied := app.recordPar2Names(jobID, jobDir, mustAssess(t, jobDir, sets, app.log), m, snap.Progress(), app.log); applied != 0 {
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
// So by the time recordPar2Names runs, looking a file up by its subject alone
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

	if applied := app.recordPar2Names(jobID, jobDir, mustAssess(t, jobDir, sets, app.log), m, snap.Progress(), app.log); applied != 1 {
		t.Fatalf("recordPar2Names recorded %d renames, want 1", applied)
	}

	after := q.SnapshotJob(jobID)
	if got := after.Progress().FileFilename(0); got != "data.bin" {
		t.Errorf("resolved filename = %q, want data.bin — the file was looked up by subject rather than by the name it actually had", got)
	}
}
