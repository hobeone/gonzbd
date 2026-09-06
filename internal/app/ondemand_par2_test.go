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
	"github.com/hobeone/gonzbd/internal/dispatch"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/nzb"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/types"
)

// copyFixturePar2 copies the shared par2 index fixture (which protects
// data.bin with CRC32 0x1068AFA6) into dir, so par2.Assess can scan it.
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

// copyFixtureSubdirPar2 copies a par2 index that protects a file inside a
// subdirectory ("Screens/data.bin").
func copyFixtureSubdirPar2(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile("../../test/fixtures/par2/subdir.par2")
	if err != nil {
		t.Skipf("subdir par2 fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subdir.par2"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// copyFixturePayload writes the bytes that match data.par2's expectation into
// dir under asName.
//
// The assessment path needs this where the old verification did not, because
// identification reads the directory — an index describing data.bin with no
// data.bin present means the file is genuinely missing, and the honest answer
// is to fetch the recovery volumes. par2Verdict itself reads only the
// assessment; it is the assessment that looks at the filesystem. The
// verification this replaced never looked at all, which is why these fixtures
// did not need a payload before.
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
// It goes through Job.SetFileCRC32FromRuns rather than writing the field,
// which means presenting the record that would have earned the value: one
// durable run at offset 0 spanning every article of the file. That is the
// gatekeeper's point — the CRC and its evidence arrive together — and it keeps
// these fixtures describing a state the program can actually reach.
func seedFileCRC(t *testing.T, j *job.Job, fileIdx int, crc uint32) {
	t.Helper()
	m, err := j.Manifest()
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
	if _, err := j.SetFileCRC32FromRuns(fileIdx, runs); err != nil {
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

// newPar2Job builds a real, not-yet-added *job.Job with OnDemandPar2 enabled.
func newPar2Job(t *testing.T, id, name string, specs []par2FileSpec) (*job.Job, dispatch.Header) {
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
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default: %v", err)
	}
	cfg.Downloads.OnDemandPar2 = true
	j, hdr, err := BuildIngestJob(cfg, parsed, "t.nzb", types.FetchOptions{JobID: id, NzbName: name}, nil)
	if err != nil {
		t.Fatalf("BuildIngestJob: %v", err)
	}
	return j, hdr
}

// buildPar2Job builds a job via newPar2Job and sets each file's assembled CRC32.
func buildPar2Job(t *testing.T, specs []par2FileSpec) *job.Job {
	t.Helper()
	j, _ := newPar2Job(t, "test-job", "test-job", specs)
	for i, f := range specs {
		if f.crc == 0 {
			continue
		}
		seedFileCRC(t, j, i, f.crc)
	}
	return j
}

// assessVerdict is what maybeReleaseRecoveryVolumes does, minus the acting on
// it: assess the directory from its current state, then read the verdict.
//
// The two used to be one call, par2Verdict, which found the par2 sets
// and ran identification and verification itself. Splitting them is the point
// of #494 — the verdict is now a pure function of an observation taken before
// anything moves — so these tests compose the two the same way production
// does rather than reaching for a combined helper that no longer exists.
func assessVerdict(t *testing.T, dir string, qjob *job.Job, log *slog.Logger) (par2Outcome, string) {
	t.Helper()
	sets, err := par2.FindPar2Files(dir, par2.DefaultParseOptions())
	if err != nil || len(sets) == 0 {
		// The caller's own fallback: no index to verify against means fetch.
		return outcomeRepair, "no usable par2 index found to verify against"
	}
	a, err := par2.AssessWithOptions(dir, sets,
		assembledFiles(mustManifest(t, qjob), qjob.Progress()), log, par2.DefaultParseOptions())
	if err != nil {
		return outcomeRepair, "could not match downloaded files against the par2 index"
	}
	return par2Verdict(a, log)
}

func TestPar2Verdict(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	deferredVol := par2FileSpec{subject: "data.vol000+01.par2", bytes: 1}

	t.Run("clean data verifies and skips recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		copyFixturePayload(t, dir, "data.bin")
		qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", crc: 0x1068AFA6, bytes: 100},
			deferredVol,
		})
		if got, _ := assessVerdict(t, dir, qjob, log); got != outcomeClean {
			t.Errorf("clean download must NOT trigger recovery-volume download, got outcome %s", got)
		}
	})

	t.Run("CRC mismatch triggers recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		copyFixturePayload(t, dir, "data.bin")
		qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", crc: 0xDEADBEEF, bytes: 100},
			deferredVol,
		})
		if got, reason := assessVerdict(t, dir, qjob, log); got != outcomeRepair {
			t.Errorf("corrupt file (CRC mismatch) must trigger recovery, got outcome %s", got)
		} else if !strings.Contains(reason, "corruption/CRC mismatch") {
			t.Errorf("expected CRC mismatch reason, got: %q", reason)
		}
	})

	t.Run("failed download (no assembled CRC) triggers recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)
		copyFixturePayload(t, dir, "data.bin")
		qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", bytes: 100},
			deferredVol,
		})
		if got, reason := assessVerdict(t, dir, qjob, log); got != outcomeRepair {
			t.Errorf("par2-tracked file with no CRC must trigger recovery, got outcome %s", got)
		} else if !strings.Contains(reason, "failed download") {
			t.Errorf("expected failed download reason, got: %q", reason)
		}
	})

	t.Run("missing par2 index falls back to fetching recovery", func(t *testing.T) {
		dir := t.TempDir() // empty — no par2 index on disk
		qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", crc: 0x1068AFA6, bytes: 100},
		})
		if got, reason := assessVerdict(t, dir, qjob, log); got != outcomeRepair {
			t.Errorf("no usable par2 index must fall back to fetching recovery volumes, got outcome %s", got)
		} else if !strings.Contains(reason, "no usable par2 index found") {
			t.Errorf("expected missing index reason, got: %q", reason)
		}
	})

	// The two ways "an entry matched nothing" can arise, which this function
	// must NOT conflate. They differ only in whether anything else matched.

	// Zero of N: no delivered file matches any entry. That is Layout B — par2
	// protecting the extracted contents — and the volumes cannot be spent,
	// because RepairStage is registered before UnpackStage (internal/app/
	// stages.go) with no second repair pass, so the only repair this pipeline
	// runs executes while the protected files do not yet exist.
	//
	// Telling this apart from a healthy obfuscated release is safe only
	// because identification is by CONTENT. Under the name-only matching this
	// PR replaces, both looked identical, and discarding here was #492.
	t.Run("no delivered file matches any entry does not fetch", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir) // protects data.bin
		// Genuinely different content, not the fixture payload under another
		// name: identification is by CONTENT, so copying data.bin to
		// other.bin would be correctly identified AS data.bin and this
		// subtest would assert the opposite of what it means to.
		if err := os.WriteFile(filepath.Join(dir, "other.bin"), []byte(strings.Repeat("not-the-protected-file", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
		qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "other.bin", crc: 0x99999999, bytes: 100},
			deferredVol,
		})
		// This used to pin the old bool `false` here — "verified clean". It no
		// longer is: "nothing matched" is not a clean verdict, it is
		// outcomeUnknown, because this same signature is also what an
		// obfuscated post damaged in its first 16 KB looks like. The
		// production behaviour this subtest cares about — the volumes are not
		// fetched — is unchanged; only the label for why is more honest.
		got, reason := assessVerdict(t, dir, qjob, log)
		if got != outcomeUnknown {
			t.Fatalf("nothing delivered matches any par2 entry, so the recovery volumes protect files this pipeline "+
				"never repairs (or the damage defeated identification); fetching them spends the whole set for no "+
				"gain, but reporting it as clean would be dishonest — got outcome %s (reason: %q)", got, reason)
		}
	})

	// Some of N: at least one delivered file IS a par2 entry, and another
	// entry is unaccounted for. Now repair is possible — the surviving files
	// supply source blocks — so the volumes must be fetched.
	//
	// This is the branch that stops the one above from becoming a blanket
	// "never fetch on an unaccounted entry", which would resurrect #492's
	// discard under a different name.
	t.Run("a partially accounted par2 set fetches recovery", func(t *testing.T) {
		dir := t.TempDir()
		copyFixturePar2(t, dir)       // protects data.bin
		copyFixtureSubdirPar2(t, dir) // protects Screens/data.bin
		copyFixturePayload(t, dir, "data.bin")

		// crc 0x99999999 is deliberately wrong (the fixture's real CRC32 is
		// 0x1068AFA6), so the identified file (data.bin) also carries a CRC
		// mismatch. That makes this fixture double as the regression case for
		// folding CRC findings into the !id.Accounted() reason below: without
		// it, every file this suite identifies while also leaving another
		// entry unaccounted verifies clean, and the fold-in code would never
		// be exercised by a non-empty CRC summary.
		qjob := buildPar2Job(t, []par2FileSpec{
			{subject: "data.bin", crc: 0x99999999, bytes: 100},
			deferredVol,
		})
		got, reason := assessVerdict(t, dir, qjob, log)
		if got != outcomeRepair {
			t.Fatalf("one entry is accounted for and another is not, so repair is possible and the volumes must "+
				"be fetched, got outcome %s", got)
		}
		if !strings.Contains(reason, "not found in this download") {
			t.Errorf("expected an unaccounted-file reason, got: %q", reason)
		}
		// The identified file's own CRC mismatch must not be discarded just
		// because another entry was unaccounted for (the bug this subtest
		// pins): both findings belong in the reason together.
		if !strings.Contains(reason, "also corruption/CRC mismatch") {
			t.Errorf("expected the identified file's CRC mismatch folded into the reason, got: %q", reason)
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

		// Stops at identification deliberately. What this pins is the
		// app-level claim against the REAL fixture: an obfuscated file is
		// accounted for by CONTENT, so the branch that discards recovery
		// volumes is never reached for a healthy release. Whether it then
		// verifies is the subject of the end-to-end test below.
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

// TestCrcVerdictParts exercises crcVerdictParts directly (rather than only
// through par2Verdict's two callers), pinning the per-category clause order
// (Mismatched, then NoCRC, then Unverified) and that a category contributes
// nothing when its count is zero.
func TestCrcVerdictParts(t *testing.T) {
	t.Parallel()

	t.Run("all categories zero produces no parts", func(t *testing.T) {
		got := crcVerdictParts(par2.CRCVerifyResult{})
		if len(got) != 0 {
			t.Fatalf("crcVerdictParts(zero value) = %v, want empty", got)
		}
	})

	t.Run("every category renders its own clause, in order", func(t *testing.T) {
		r := par2.CRCVerifyResult{
			Mismatched: 1,
			Files:      []par2.CRCResult{{FileName: "bad.rar", Match: false}},
			NoCRC:      2,
			NoCRCFiles: []string{"a.rar", "b.rar"},
			Unverified: 3,
		}
		got := crcVerdictParts(r)
		want := []string{
			"corruption/CRC mismatch in 1 file(s) (bad.rar)",
			"failed download in 2 file(s) (a.rar, b.rar)",
			"3 file(s) unverified",
		}
		if len(got) != len(want) {
			t.Fatalf("crcVerdictParts(%+v) = %v, want %v", r, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("part %d = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

func TestPar2Verdict_NothingIdentifiedIsNotACleanVerdict(t *testing.T) {
	t.Parallel()

	// Nothing on disk matched any par2 entry. That is a Layout B post — and
	// it is ALSO an obfuscated single-file post damaged inside its first
	// 16 KB, which defeats identification passes 1, 2 and 3 together. The
	// two are indistinguishable from this value, so it cannot be reported as
	// a clean verdict.
	//
	// Holding the volumes does NOT rescue the damaged case: nothing promotes
	// a held volume after finalize (undeferRecovery has two callers, neither
	// reachable post-finalize) and ResetForRetry downgrades FetchNever to
	// FetchIfNeeded anyway. What the hold buys is an honest label — fileState
	// renders FetchIfNeeded as "held" and FetchNever as "skipped"
	// (internal/api/queue.go:327-329), and "skipped" claims a verdict that
	// was never earned.
	a := par2.Assessment{
		ID:  par2.Identification{Unaccounted: []par2.FileDesc{{FileName: "payload.mkv"}}},
		CRC: par2.CRCVerifyResult{Unverified: 1},
	}

	got, reason := par2Verdict(a, slog.New(slog.DiscardHandler))
	if got != outcomeUnknown {
		t.Fatalf("outcome = %s, want %s", got, outcomeUnknown)
	}
	if reason == "" {
		t.Error("reason is empty; nothing records why the volumes were held")
	}
}

func TestMaybeReleaseRecoveryVolumes(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()

	app := newTestApplication(t)
	app.config.With(func(c *config.Config) {
		c.General.DownloadDir = dir
		c.Downloads.OnDemandPar2 = true
	})
	app.log = log

	const jobID = "job-1"
	qjob, hdr := newPar2Job(t, jobID, "job-name", []par2FileSpec{
		{subject: "data.bin", bytes: 100},
		{subject: "data.vol000+01.par2", bytes: 100},
	})
	if err := app.Dispatcher().Add(qjob, hdr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	seedFileCRC(t, qjob, 0, 0x1068AFA6)

	t.Run("context cancelled returns false", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(t.Context())
		cancel()

		if app.maybeReleaseRecoveryVolumes(cancelledCtx, jobID) {
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

		if app.maybeReleaseRecoveryVolumes(t.Context(), jobID) {
			t.Error("maybeReleaseRecoveryVolumes must return false when verification is clean")
		}

		// Verify that the recovery volume is marked FetchNever rather than
		// removed from the job: DiscardDeferredPar2 no longer changes the
		// file set, so the recovery volume is still in the manifest, just no
		// longer awaiting the CRC verdict.
		j, ok := app.Dispatcher().Job(jobID)
		if !ok {
			t.Fatal("job not in dispatcher")
		}
		m := mustManifest(t, j)
		sawRecovery := false
		for fi := range m.NumFiles() {
			if !m.FileIsPar2Recovery(fi) {
				continue
			}
			sawRecovery = true
			if got := j.Progress().FileFetchPolicy(fi); got != job.FetchNever {
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
		jobCorrupt, hdrCorrupt := newPar2Job(t, jobCorruptID, "job-corrupt-name", []par2FileSpec{
			{subject: "data.bin", bytes: 100},
			{subject: "data.vol000+01.par2", bytes: 100},
		})
		seedFileCRC(t, jobCorrupt, 0, 0xDEADBEEF)
		if err := app.Dispatcher().Add(jobCorrupt, hdrCorrupt); err != nil {
			t.Fatal(err)
		}

		dirCorrupt := filepath.Join(dir, "job-corrupt-name")
		if err := os.MkdirAll(dirCorrupt, 0o750); err != nil {
			t.Fatal(err)
		}
		copyFixturePar2(t, dirCorrupt)
		copyFixturePayload(t, dirCorrupt, "data.bin")

		if !app.maybeReleaseRecoveryVolumes(t.Context(), jobCorruptID) {
			t.Error("maybeReleaseRecoveryVolumes must return true when verification fails")
		}

		// Verify that the deferred recovery volume was undeferred.
		m, p := mustManifest(t, jobCorrupt), jobCorrupt.Progress()
		found := false
		for fi := range m.NumFiles() {
			if m.FileIsPar2Recovery(fi) {
				found = true
				if p.FileFetchPolicy(fi) != job.FetchAlways {
					t.Error("deferred recovery volume was not undeferred")
				}
			}
		}
		if !found {
			t.Error("recovery volume disappeared from job")
		}
	})

	// The branch that runs when there is no index to assess against.
	t.Run("no par2 index on disk fetches the volumes", func(t *testing.T) {
		const noIndexJobID = "job-no-index"

		noIdx, hdrNoIdx := newPar2Job(t, noIndexJobID, "job-no-index-name", []par2FileSpec{
			{subject: "data.bin", bytes: 100},
			{subject: "data.vol000+01.par2", bytes: 100},
		})
		seedFileCRC(t, noIdx, 0, 0x1068AFA6)
		if err := app.Dispatcher().Add(noIdx, hdrNoIdx); err != nil {
			t.Fatal(err)
		}

		// The job directory exists but holds no par2 files at all.
		if err := os.MkdirAll(filepath.Join(dir, "job-no-index-name"), 0o750); err != nil {
			t.Fatal(err)
		}
		copyFixturePayload(t, filepath.Join(dir, "job-no-index-name"), "data.bin")

		if !app.maybeReleaseRecoveryVolumes(t.Context(), noIndexJobID) {
			t.Error("a job whose par2 index never arrived must fetch its recovery volumes; without an index " +
				"nothing can be verified, so skipping them would ship an unchecked job")
		}
	})

	t.Run("a clean obfuscated download fetches nothing", func(t *testing.T) {
		const obfJobID = "job-obfuscated"
		const obfuscated = "7xq6N6P340dCh9Lnih5hY3jsArfSN1"

		obfJob, hdrObf := newPar2Job(t, obfJobID, "job-obfuscated-name", []par2FileSpec{
			{subject: obfuscated, bytes: 100},
			{subject: "data.vol000+01.par2", bytes: 100},
		})
		seedFileCRC(t, obfJob, 0, 0x1068AFA6)
		if err := app.Dispatcher().Add(obfJob, hdrObf); err != nil {
			t.Fatal(err)
		}

		dirObf := filepath.Join(dir, "job-obfuscated-name")
		if err := os.MkdirAll(dirObf, 0o750); err != nil {
			t.Fatal(err)
		}
		copyFixturePar2(t, dirObf) // protects data.bin
		copyFixturePayload(t, dirObf, obfuscated)

		if app.maybeReleaseRecoveryVolumes(t.Context(), obfJobID) {
			t.Fatal("an intact obfuscated download undeferred its recovery volumes; identification finds the file " +
				"by content, so a verdict of \"needs repair\" means verification was not reading what " +
				"identification found")
		}

		if _, err := os.Stat(filepath.Join(dirObf, obfuscated)); err != nil {
			t.Errorf("the delivered file is no longer at %q: the download path must not move files, or the "+
				"queue's record of where they are stops being true: %v", obfuscated, err)
		}
		if _, err := os.Stat(filepath.Join(dirObf, "data.bin")); err == nil {
			t.Error("the download path renamed a file to its par2 name; that belongs to post-processing, and " +
				"doing it here cannot be recorded truthfully for a subdirectory target")
		}
	})
}
