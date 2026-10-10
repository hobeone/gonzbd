package postproc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/job/jobtest"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/types"
)

// deliveredFile is one file a test job downloaded: its bytes and the name it
// was written under.
type deliveredFile struct {
	name string
	data []byte
}

// fixtureBytes reads a file under test/fixtures/par2/<layout>/.
func fixtureBytes(t *testing.T, layout, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(par2LayoutFixture(layout, name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// deliveredJob writes each delivered file into a fresh download directory,
// copies the named fixture files beside them, and returns a PP=3 job whose
// manifest lists every delivered file with the CRC of the bytes delivered.
func deliveredJob(t *testing.T, layout string, fixtures []string, delivered []deliveredFile) (*Job, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range fixtures {
		copyToDir(t, par2LayoutFixture(layout, name), dir)
	}
	files := make([]job.JobFile, len(delivered))
	for i, d := range delivered {
		if err := os.WriteFile(filepath.Join(dir, d.name), d.data, 0o644); err != nil {
			t.Fatal(err)
		}
		files[i] = job.JobFile{
			Subject:  d.name,
			Bytes:    int64(len(d.data)),
			Articles: []job.JobArticle{{ID: d.name + "-a@t", Bytes: len(d.data)}},
		}
	}
	j := job.New("delivered-job", "delivered-job.nzb", job.Policy{})
	if err := j.AttachContent(job.NewManifest(files)); err != nil {
		t.Fatalf("AttachContent: %v", err)
	}
	for i, d := range delivered {
		if err := j.SetFileFilename(i, d.name); err != nil {
			t.Fatalf("SetFileFilename: %v", err)
		}
		jobtest.SeedFileCRC(t, j, i, crc32.ChecksumIEEE(d.data))
	}
	return &Job{Job: j, DownloadDir: dir, PP: types.PPDelete}, dir
}

// assertFeatureExtracted fails unless dir holds feature.bin with the bytes
// the Layout B fixture's archive was built from.
func assertFeatureExtracted(t *testing.T, dir string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "feature.bin"))
	if err != nil {
		t.Fatalf("feature.bin was not extracted: %v", err)
	}
	wantLine := fixtureBytes(t, "layout_b", "feature.bin.sha256")
	sum := sha256.Sum256(got)
	if want := strings.Fields(string(wantLine))[0]; hex.EncodeToString(sum[:]) != want {
		t.Errorf("feature.bin sha256 = %x, want %s: the extracted file is not what par2 protects", sum, want)
	}
}

// par2Stages returns the par2-related stages in pipeline order, wired as
// buildStages wires them: quickcheck, repair, unpack, extracted_repair and
// par2_cleanup.
func par2Stages() []Stage {
	qc, repair, up := layoutStages()
	extracted := NewExtractedRepairStage(repair)
	extracted.Log = slog.New(slog.DiscardHandler)
	cleanup := NewPar2CleanupStage(true)
	cleanup.Log = slog.New(slog.DiscardHandler)
	return []Stage{qc, repair, up, extracted, cleanup}
}

// damagedNoDigest is a stored RAR5 of feature.bin whose file header records no
// digest at all, with four bytes of the member overwritten. go_rar has nothing
// to check the content against (rarengine reports ErrChecksumUnsupported, which
// the extractor filters), so the damage extracts without an error; the one
// recovery block in feature.vol0+1.par2 covers it. It used to record a BLAKE2sp
// digest instead, but rarengine verifies those now and would catch the damage.
func damagedNoDigest(t *testing.T) deliveredFile {
	t.Helper()
	return deliveredFile{name: "release.rar", data: fixtureBytes(t, "layout_b", "damaged_nodigest.rar")}
}

func assertAbsent(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still in the download directory (stat err %v)", name, err)
		}
	}
}

func assertPresent(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is gone from the download directory: %v", name, err)
		}
	}
}

// The archive's checksum is the only check a Layout B post's extraction gets
// unless par2 is run against what it extracted. Here the archive cannot check
// its member, so only par2 sees the damage, and its recovery block repairs it.
// Once par2 has verified the extracted file, its set may be cleaned up.
func TestLayoutB_ExtractedFileIsVerifiedAgainstPar2(t *testing.T) {
	t.Parallel()

	job, dir := deliveredJob(t, "layout_b", layoutBPar2, []deliveredFile{damagedNoDigest(t)})

	stageErrs := runStages(t, job, par2Stages()...)

	if job.QuickCheck != QuickCheckUnidentified {
		t.Fatalf("QuickCheck = %s, want unidentified", job.QuickCheck)
	}
	if job.UnpackError {
		t.Fatalf("UnpackError = true: the archive caught its own damage, so this test no longer shows what par2 adds; stage errors: %v", stageErrs)
	}
	if job.ParError {
		t.Errorf("ParError = true; the recovery block covers the damage. stage errors: %v", stageErrs)
	}
	assertFeatureExtracted(t, dir)
	if !job.DeferredPar2Verified {
		t.Error("DeferredPar2Verified = false after extracted_repair repaired the set")
	}
	assertAbsent(t, dir, layoutBPar2...)
}

// Without the recovery volume the same damage cannot be repaired, and the job
// must fail rather than ship the damaged file; the par2 set is kept.
func TestLayoutB_UnrepairableExtractionFailsTheJob(t *testing.T) {
	t.Parallel()

	job, dir := deliveredJob(t, "layout_b", []string{"feature.par2"}, []deliveredFile{damagedNoDigest(t)})

	stageErrs := runStages(t, job, par2Stages()...)

	if job.UnpackError {
		t.Fatalf("UnpackError = true: the archive caught its own damage; stage errors: %v", stageErrs)
	}
	if !job.ParError {
		t.Fatalf("ParError = false with a damaged extraction and no recovery block (QuickCheck=%s); stage errors: %v",
			job.QuickCheck, stageErrs)
	}
	if job.DeferredPar2Verified {
		t.Error("DeferredPar2Verified = true for a set that failed")
	}
	if summary := buildSummaryEntry(job); !strings.HasPrefix(summary.Lines[0], "Pipeline Failed") {
		t.Errorf("summary = %q, want the job Failed", summary.Lines[0])
	}
	assertPresent(t, dir, "feature.par2")
}

// A par2 set that also protects a delivered sidecar identifies it by name, so
// the set is not "nothing identified". The entry that was not accounted for is
// still the archive's member, and the set must still wait for unpack.
func TestLayoutB_IdentifiedSidecarStillDefersToUnpack(t *testing.T) {
	t.Parallel()

	job, dir := deliveredJob(t, "layout_b_mixed",
		[]string{"withnfo.par2", "withnfo.vol0+1.par2"},
		[]deliveredFile{
			{name: "release.rar", data: fixtureBytes(t, "layout_b", "release.rar")},
			{name: "feature.nfo", data: fixtureBytes(t, "layout_b_mixed", "feature.nfo")},
		})

	stageErrs := runStages(t, job, par2Stages()...)

	if job.ParError || job.UnpackError {
		t.Fatalf("ParError = %v, UnpackError = %v (QuickCheck=%s): a repair ran before the files it checks were extracted; stage errors: %v",
			job.ParError, job.UnpackError, job.QuickCheck, stageErrs)
	}
	if job.QuickCheck != QuickCheckUnidentified {
		t.Errorf("QuickCheck = %s, want unidentified: the job's one set is deferred", job.QuickCheck)
	}
	assertFeatureExtracted(t, dir)
	if !job.DeferredPar2Verified {
		t.Error("DeferredPar2Verified = false: the deferred set was not verified after unpack")
	}
}

// A Layout B set beside an ordinary set: the ordinary set's file is delivered
// damaged and must be repaired before unpack, while the Layout B set waits for
// unpack. Judging the job as a whole either skips the first repair or runs the
// second one early.
func TestLayoutB_SetBesideAnOrdinarySetDefersOnlyItself(t *testing.T) {
	t.Parallel()

	extras := fixtureBytes(t, "layout_b_mixed", "extras.txt")
	damaged := append([]byte(nil), extras...)
	damaged[3] ^= 0xff
	job, dir := deliveredJob(t, "layout_b_mixed",
		[]string{"extras.par2", "extras.vol0+1.par2"},
		[]deliveredFile{
			{name: "release.rar", data: fixtureBytes(t, "layout_b", "release.rar")},
			{name: "extras.txt", data: damaged},
		})
	for _, name := range layoutBPar2 {
		copyToDir(t, par2LayoutFixture("layout_b", name), dir)
	}
	stages := par2Stages()

	// quickcheck and repair alone: the ordinary set is repaired now, and the
	// Layout B set is left for after unpack.
	stageErrs := runStages(t, job, stages[:2]...)
	if job.QuickCheck != QuickCheckDamaged {
		t.Errorf("QuickCheck = %s, want damaged: the verdict is the ordinary set's", job.QuickCheck)
	}
	if !slices.Equal(job.DeferredPar2Sets, []string{"feature"}) {
		t.Errorf("DeferredPar2Sets = %v, want [feature]", job.DeferredPar2Sets)
	}
	got, err := os.ReadFile(filepath.Join(dir, "extras.txt"))
	if err != nil || string(got) != string(extras) {
		t.Errorf("extras.txt = %q (err %v), want the ordinary set's repair to restore %q", got, err, extras)
	}

	stageErrs = append(stageErrs, runStages(t, job, stages[2:]...)...)
	if job.ParError || job.UnpackError {
		t.Fatalf("ParError = %v, UnpackError = %v (QuickCheck=%s); stage errors: %v",
			job.ParError, job.UnpackError, job.QuickCheck, stageErrs)
	}
	assertFeatureExtracted(t, dir)
	if !job.DeferredPar2Verified {
		t.Error("DeferredPar2Verified = false: the deferred set was not verified after unpack")
	}
}

// extracted_repair has nothing to do for a job with no deferred set, and must
// not run par2 after a failed repair or unpack: the files it would check were
// not extracted, or not completely.
func TestExtractedRepairStage_Skips(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*Job)
	}{
		{"no deferred set", func(j *Job) { j.DeferredPar2Sets = nil }},
		{"repair failed", func(j *Job) { j.ParError = true }},
		{"unpack failed", func(j *Job) { j.UnpackError = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			job, _ := deliveredJob(t, "layout_b", layoutBPar2, []deliveredFile{damagedNoDigest(t)})
			job.DeferredPar2Sets = []string{"feature"}
			tc.mutate(job)
			parErr := job.ParError
			_, repair, _ := layoutStages()
			if err := NewExtractedRepairStage(repair).Run(t.Context(), job); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if job.DeferredPar2Verified {
				t.Error("DeferredPar2Verified = true for a job it skipped")
			}
			if job.ParError != parErr {
				t.Errorf("ParError = %v, want it left at %v", job.ParError, parErr)
			}
			for _, line := range job.OutputLines {
				if strings.HasPrefix(line, "[par2]") {
					t.Errorf("par2 ran: %q", line)
				}
			}
		})
	}
}

// A deferred set that is gone by the time unpack has finished verified
// nothing, so the job fails rather than report its files checked.
func TestExtractedRepairStage_MissingDeferredSetFailsTheJob(t *testing.T) {
	t.Parallel()

	job, _ := deliveredJob(t, "layout_b", layoutBPar2, []deliveredFile{damagedNoDigest(t)})
	job.DeferredPar2Sets = []string{"feature", "vanished"}
	_, repair, _ := layoutStages()

	err := NewExtractedRepairStage(repair).Run(t.Context(), job)

	if err == nil || !job.ParError {
		t.Errorf("Run = %v, ParError = %v; want an error and ParError for a deferred set that is not there", err, job.ParError)
	}
	if job.DeferredPar2Verified {
		t.Error("DeferredPar2Verified = true with a deferred set unverified")
	}
}

// The verdict is read over the sets that were not deferred. Here that is one
// intact file, so the job is Clean and repair is skipped, though the deferred
// set's entry was identified as nothing.
func TestQuickCheckStage_VerdictExcludesDeferredSets(t *testing.T) {
	t.Parallel()

	job, dir := deliveredJob(t, "layout_b_mixed",
		[]string{"extras.par2", "extras.vol0+1.par2"},
		[]deliveredFile{
			{name: "release.rar", data: fixtureBytes(t, "layout_b", "release.rar")},
			{name: "extras.txt", data: fixtureBytes(t, "layout_b_mixed", "extras.txt")},
		})
	for _, name := range layoutBPar2 {
		copyToDir(t, par2LayoutFixture("layout_b", name), dir)
	}
	qc, _, _ := layoutStages()

	runQuickCheck(t, qc, job)

	if job.QuickCheck != QuickCheckClean {
		t.Errorf("QuickCheck = %s, want clean: the one set not deferred verified", job.QuickCheck)
	}
	if !slices.Equal(job.DeferredPar2Sets, []string{"feature"}) {
		t.Errorf("DeferredPar2Sets = %v, want [feature]", job.DeferredPar2Sets)
	}
}

// repair leaves a deferred set alone and runs every other one.
func TestRepairStage_SkipsDeferredSets(t *testing.T) {
	t.Parallel()

	job, _ := deliveredJob(t, "layout_b", layoutBPar2, []deliveredFile{damagedNoDigest(t)})
	job.QuickCheck = QuickCheckDamaged
	job.DeferredPar2Sets = []string{"feature"}
	_, repair, _ := layoutStages()

	if err := repair.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.ParError {
		t.Error("ParError = true: repair ran a set quickcheck deferred until after unpack")
	}
	for _, line := range job.OutputLines {
		if strings.HasPrefix(line, "[par2]") {
			t.Errorf("par2 ran on a deferred set: %q", line)
		}
	}
}

// repairSets runs exactly the sets on the side of the deferral it is asked
// for, and reports how many it ran.
func TestRepairStage_RepairSets(t *testing.T) {
	t.Parallel()

	newJob := func(t *testing.T) *Job {
		t.Helper()
		job, dir := deliveredJob(t, "layout_b_mixed",
			[]string{"extras.par2", "extras.vol0+1.par2"},
			[]deliveredFile{{name: "extras.txt", data: fixtureBytes(t, "layout_b_mixed", "extras.txt")}})
		for _, name := range layoutBPar2 {
			copyToDir(t, par2LayoutFixture("layout_b", name), dir)
		}
		job.DeferredPar2Sets = []string{"feature"}
		return job
	}
	ranSets := func(job *Job) []string {
		var sets []string
		for _, line := range job.OutputLines {
			if name, ok := strings.CutPrefix(line, "[par2] "); ok {
				sets = append(sets, name)
			}
		}
		return sets
	}
	log := slog.New(slog.DiscardHandler)

	for _, tc := range []struct {
		deferred bool
		want     string
	}{
		{false, "extras"},
		{true, "feature"},
	} {
		job := newJob(t)
		_, repair, _ := layoutStages()
		ran, err := repair.repairSets(t.Context(), log, job, tc.deferred)
		// The feature set has nothing extracted to check, so running it fails;
		// only the count and the set run matter here.
		if !tc.deferred && err != nil {
			t.Errorf("deferred=false: err = %v", err)
		}
		if got := ranSets(job); ran != 1 || !slices.Equal(got, []string{tc.want}) {
			t.Errorf("deferred=%v: ran %d set(s) %v, want 1: [%s]", tc.deferred, ran, got, tc.want)
		}
	}

	t.Run("no par2 files", func(t *testing.T) {
		t.Parallel()
		job, _ := stageJob(t)
		_, repair, _ := layoutStages()
		if ran, err := repair.repairSets(t.Context(), log, job, true); ran != 0 || err != nil || job.ParError {
			t.Errorf("ran = %d, err = %v, ParError = %v; want nothing run and no error", ran, err, job.ParError)
		}
	})
	t.Run("unreadable directory", func(t *testing.T) {
		t.Parallel()
		job, dir := stageJob(t)
		job.DownloadDir = filepath.Join(dir, "absent")
		_, repair, _ := layoutStages()
		if ran, err := repair.repairSets(t.Context(), log, job, true); ran != 0 || err == nil || !job.ParError {
			t.Errorf("ran = %d, err = %v, ParError = %v; want an error and ParError", ran, err, job.ParError)
		}
	})
}

func TestSetsWithEntries(t *testing.T) {
	t.Parallel()

	id := par2.Identification{
		Files:       []par2.Identified{{Desc: par2.FileDesc{FileName: "a.nfo", Set: "a"}}},
		Unaccounted: []par2.FileDesc{{FileName: "b.mkv", Set: "b"}, {FileName: "a.mkv", Set: "a"}},
	}
	got := setsWithEntries(id)
	if len(got) != 2 || !got["a"] || !got["b"] {
		t.Errorf("setsWithEntries = %v, want a and b", got)
	}
	if got := setsWithEntries(par2.Identification{}); len(got) != 0 {
		t.Errorf("setsWithEntries of nothing = %v, want empty", got)
	}
}

func TestJob_Par2Deferred(t *testing.T) {
	t.Parallel()

	job := &Job{DeferredPar2Sets: []string{"feature"}}
	if !job.par2Deferred("feature") || job.par2Deferred("extras") {
		t.Errorf("par2Deferred(feature, extras) = %v, %v; want true, false",
			job.par2Deferred("feature"), job.par2Deferred("extras"))
	}
}

// An archive that records a plain BLAKE2sp digest checks its own member: the
// Go path now verifies it, so damage that used to slip through to par2 is
// reported as an extraction failure. damaged_nodigest.rar is what still
// reaches extracted_repair.
func TestLayoutB_Blake2spDamageIsCaughtByTheArchive(t *testing.T) {
	t.Parallel()

	job, _ := deliveredJob(t, "layout_b", layoutBPar2, []deliveredFile{
		{name: "release.rar", data: fixtureBytes(t, "layout_b", "damaged_b2.rar")},
	})

	stageErrs := runStages(t, job, par2Stages()...)

	if !job.UnpackError {
		t.Fatalf("UnpackError = false: the damaged BLAKE2sp archive extracted without complaint; stage errors: %v", stageErrs)
	}
	if !strings.Contains(fmt.Sprint(stageErrs), "checksum") {
		t.Errorf("stage errors do not report the checksum mismatch: %v", stageErrs)
	}
}
