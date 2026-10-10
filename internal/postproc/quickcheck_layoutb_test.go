package postproc

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// par2LayoutFixture names a file under test/fixtures/par2/<layout>/.
//
// layout_b is a Layout B post: release.rar (and release.7z, the same content)
// extract to feature.bin, and feature.par2 + feature.vol0+1.par2 protect
// feature.bin. No delivered file is anything that par2 set names.
//
// layout_a protects an ARCHIVE: Real.Name.par2 + Real.Name.vol0+2.par2 were
// created over Real.Name.rar. d8f7a6.rar is that archive under an obfuscated
// name with four bytes overwritten inside its first 16 KB, which the recovery
// volume can repair. outer.rar is a RAR whose one member is Real.Name.rar.
func par2LayoutFixture(layout, name string) string {
	return filepath.Join("..", "..", "test", "fixtures", "par2", layout, name)
}

// layoutJob copies the named par2 files and one payload into a fresh download
// directory, the payload under payloadName, and returns a PP=3 job whose
// manifest lists that payload with its correct assembled CRC.
func layoutJob(t *testing.T, layout string, par2Files []string, payloadSrc, payloadName string) (*Job, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range par2Files {
		copyToDir(t, par2LayoutFixture(layout, name), dir)
	}
	payload, err := os.ReadFile(payloadSrc)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, payloadName), payload, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	j := buildQCJob(t, "layout-job", payloadName, int64(len(payload)), crc32.ChecksumIEEE(payload))
	return &Job{Job: j, DownloadDir: dir, FinalDir: dir, PP: types.PPDelete}, dir
}

var layoutBPar2 = []string{"feature.par2", "feature.vol0+1.par2"}

// layoutBJob is the Layout B post with release.rar delivered as payloadName.
func layoutBJob(t *testing.T, payloadName string) (*Job, string) {
	t.Helper()
	return layoutJob(t, "layout_b", layoutBPar2, par2LayoutFixture("layout_b", "release.rar"), payloadName)
}

// layoutStages returns quickcheck wired to an enabled native unpack stage, as
// buildStages wires them, and a native repair stage.
func layoutStages() (*QuickCheckStage, *RepairStage, *UnpackStage) {
	up := NewUnpackStageWith(unpack.Options{UseGoRAR: true, UseGo7z: true}, false)
	up.SetEnabled(true)
	qc := &QuickCheckStage{Log: slog.New(slog.DiscardHandler), Unpack: up}
	qc.SetEnabled(true)
	repair := &RepairStage{UseGoPar2: true, Log: slog.New(slog.DiscardHandler)}
	return qc, repair, up
}

// runStages runs stages in order the way the runner does: an error is
// recorded and the next stage still runs.
func runStages(t *testing.T, job *Job, stages ...Stage) []string {
	t.Helper()
	var errs []string
	for _, s := range stages {
		if err := s.Run(t.Context(), job); err != nil {
			errs = append(errs, s.Name()+": "+err.Error())
		}
	}
	return errs
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runQuickCheck(t *testing.T, qc *QuickCheckStage, job *Job) {
	t.Helper()
	if err := qc.Run(t.Context(), job); err != nil {
		t.Fatalf("quickcheck: %v", err)
	}
}

// A Layout B post is healthy and extractable, and the pipeline must extract
// it. Its par2 set describes files that do not exist until unpack has run, so
// a repair attempted before unpack fails on this compressed archive — and a
// failed repair skips unpack unconditionally. With no extracted_repair in
// these stages to verify the extracted file, par2_cleanup keeps the par2 set.
func TestLayoutB_ArchiveExtractsDespitePar2NamingItsContents(t *testing.T) {
	t.Parallel()

	job, dir := layoutBJob(t, "release.rar")
	qc, repair, up := layoutStages()
	cleanup := NewPar2CleanupStage(true)
	cleanup.Log = slog.New(slog.DiscardHandler)
	finalize := NewFinalizeStage()
	finalize.Log = slog.New(slog.DiscardHandler)

	stageErrs := runStages(t, job, qc, repair, up, cleanup, finalize)

	if job.ParError {
		t.Fatalf("ParError = true after a repair of files that do not exist yet (QuickCheck=%s), so unpack was skipped; stage errors: %v",
			job.QuickCheck, stageErrs)
	}
	if job.UnpackError {
		t.Fatal("UnpackError = true; want a clean extraction")
	}
	if len(stageErrs) > 0 {
		t.Errorf("stage errors: %v", stageErrs)
	}
	got, err := os.ReadFile(filepath.Join(dir, "feature.bin"))
	if err != nil {
		t.Fatalf("the archive was not extracted: %v", err)
	}
	wantLine, err := os.ReadFile(par2LayoutFixture("layout_b", "feature.bin.sha256"))
	if err != nil {
		t.Fatalf("read fixture checksum: %v", err)
	}
	sum := sha256.Sum256(got)
	if want := strings.Fields(string(wantLine))[0]; hex.EncodeToString(sum[:]) != want {
		t.Errorf("extracted feature.bin sha256 = %x, want %s", sum, want)
	}
	for _, name := range layoutBPar2 {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("par2_cleanup deleted %s, whose deferred set nothing verified: %v", name, err)
		}
	}
}

// The load-bearing case for declining repair: the archive's own checksums
// are the first check on the extraction, so a damaged archive under
// Unidentified has to end the job Failed through UnpackError — never a clean
// success.
func TestLayoutB_DamagedArchiveFailsTheJob(t *testing.T) {
	t.Parallel()

	job, dir := layoutBJob(t, "release.rar")
	// Overwrite packed bytes in the middle of the member, away from the
	// headers at either end, so the members still list and extraction fails.
	path := filepath.Join(dir, "release.rar")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(data) / 2
	for i := mid; i < mid+16; i++ {
		data[i] ^= 0xff
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	qc, repair, up := layoutStages()

	runStages(t, job, qc, repair, up)

	if job.QuickCheck != QuickCheckUnidentified {
		t.Fatalf("QuickCheck = %s, want unidentified: this test is about what stands in for the skipped repair", job.QuickCheck)
	}
	if job.ParError {
		t.Error("ParError = true; repair should have been skipped")
	}
	if !job.UnpackError {
		t.Error("UnpackError = false for a damaged archive whose par2 set nothing checked")
	}
	if summary := buildSummaryEntry(job); !strings.HasPrefix(summary.Lines[0], "Pipeline Failed") {
		t.Errorf("summary = %q, want the job Failed", summary.Lines[0])
	}
}

// Each condition of the classification, one case at a time. Every case but
// the two genuine Layout B archives must stay Damaged, so repair runs.
func TestQuickCheckStage_UnidentifiedNeedsEveryCondition(t *testing.T) {
	t.Parallel()

	layoutB7z := par2LayoutFixture("layout_b", "release.7z")
	layoutBRar := par2LayoutFixture("layout_b", "release.rar")
	cases := []struct {
		name    string
		layout  string
		par2    []string
		src     string
		payload string
		// mutate adjusts the job or stages after they are built.
		mutate func(*testing.T, *Job, *QuickCheckStage)
		want   QuickCheckOutcome
	}{
		{name: "rar holding the protected file", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.rar", want: QuickCheckUnidentified},
		{name: "7z holding the protected file", layout: "layout_b", par2: layoutBPar2, src: layoutB7z, payload: "release.7z", want: QuickCheckUnidentified},

		// Nothing verifies a plain file's extraction, a split join or a tar.
		{name: "no archive", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.bin", want: QuickCheckDamaged},
		{name: "split join", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.001", want: QuickCheckDamaged},
		{name: "tar", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.tar", want: QuickCheckDamaged},

		// A split join beside the archive is not listed — it has no members to
		// list — so it neither helps nor blocks the archive that holds the file.
		{name: "rar holding the file beside a split join", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.rar",
			mutate: func(t *testing.T, j *Job, _ *QuickCheckStage) {
				writeFile(t, filepath.Join(j.DownloadDir, "extras.001"), "split")
			},
			want: QuickCheckUnidentified},
		// Any archive that cannot be listed fails the check closed, even when
		// another archive holds every entry.
		{name: "second archive cannot be listed", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.rar",
			mutate: func(t *testing.T, j *Job, _ *QuickCheckStage) {
				writeFile(t, filepath.Join(j.DownloadDir, "broken.rar"), "not a rar")
			},
			want: QuickCheckDamaged},

		// Unpack must run, or declining repair verifies nothing.
		{name: "PP repair only", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.rar",
			mutate: func(_ *testing.T, j *Job, _ *QuickCheckStage) { j.PP = types.PPVerify }, want: QuickCheckDamaged},
		{name: "unpack disabled", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.rar",
			mutate: func(_ *testing.T, _ *Job, q *QuickCheckStage) { q.Unpack.SetEnabled(false) }, want: QuickCheckDamaged},
		{name: "unpack not wired", layout: "layout_b", par2: layoutBPar2, src: layoutBRar, payload: "release.rar",
			mutate: func(_ *testing.T, _ *Job, q *QuickCheckStage) { q.Unpack = nil }, want: QuickCheckDamaged},

		// An archive that does not name the protected file says nothing
		// about it: here par2 protects feature.bin, and the only archive
		// holds file1.txt, file2.txt and subdir/nested.txt.
		{name: "unrelated archive", layout: "layout_b", par2: layoutBPar2, src: unpackFixture("single_rar5.rar"), payload: "Subs.rar", want: QuickCheckDamaged},

		// par2 names an archive, so the set protects archives (Layout A).
		// outer.rar holds Real.Name.rar, so only the entry's name decides.
		{name: "par2 names an archive", layout: "layout_a", par2: []string{"Real.Name.par2", "Real.Name.vol0+2.par2"},
			src: par2LayoutFixture("layout_a", "outer.rar"), payload: "outer.rar", want: QuickCheckDamaged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			job, _ := layoutJob(t, tc.layout, tc.par2, tc.src, tc.payload)
			qc, _, _ := layoutStages()
			if tc.mutate != nil {
				tc.mutate(t, job, qc)
			}
			runQuickCheck(t, qc, job)
			if job.QuickCheck != tc.want {
				t.Errorf("QuickCheck = %s, want %s", job.QuickCheck, tc.want)
			}
		})
	}
}

// At PP=1 unpack never runs, so repair is the only check the job gets; it
// must run and its failure must stand, not be skipped into a success with
// nothing verified.
func TestLayoutB_RepairOnlyJobKeepsRepair(t *testing.T) {
	t.Parallel()

	job, _ := layoutBJob(t, "release.rar")
	job.PP = types.PPVerify
	qc, repair, _ := layoutStages()

	runStages(t, job, qc, repair)

	if job.QuickCheck != QuickCheckDamaged {
		t.Errorf("QuickCheck = %s at PP=%d, want damaged", job.QuickCheck, job.PP)
	}
	if !job.ParError {
		t.Error("ParError = false at PP=1: repair was skipped and nothing verified the job")
	}
}

// An obfuscated single archive damaged inside its first 16 KB matches no
// par2 entry either. Its par2 set protects the archive itself, and a repair
// succeeds — so it must not be classified Unidentified.
func TestLayoutA_ObfuscatedDamagedArchiveIsRepaired(t *testing.T) {
	t.Parallel()

	job, dir := layoutJob(t, "layout_a", []string{"Real.Name.par2", "Real.Name.vol0+2.par2"},
		par2LayoutFixture("layout_a", "d8f7a6.rar"), "d8f7a6.rar")
	qc, repair, _ := layoutStages()

	stageErrs := runStages(t, job, qc, repair)

	if job.QuickCheck != QuickCheckDamaged {
		t.Fatalf("QuickCheck = %s, want damaged", job.QuickCheck)
	}
	if job.ParError {
		t.Fatalf("ParError = true; the recovery volume covers the damage. stage errors: %v", stageErrs)
	}
	if _, err := os.Stat(filepath.Join(dir, "Real.Name.rar")); err != nil {
		t.Errorf("repair did not reconstruct Real.Name.rar: %v", err)
	}
}

// Identifying a file in the directory means the par2 set describes something
// that is there to check, so repair has work to do and the archive beside it
// does not change that. It must stay Damaged.
func TestQuickCheckStage_IdentifiedEntryStaysDamaged(t *testing.T) {
	t.Parallel()

	job, dir := layoutBJob(t, "release.rar")
	// Put the protected file in the directory too, under its own name but
	// truncated, so the set identifies it by name and still has nothing to
	// verify it by: the manifest lists only release.rar.
	if err := os.WriteFile(filepath.Join(dir, "feature.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	qc, _, _ := layoutStages()
	runQuickCheck(t, qc, job)
	if job.QuickCheck != QuickCheckDamaged {
		t.Errorf("QuickCheck = %s with a par2 entry identified in the directory, want damaged", job.QuickCheck)
	}
}

// unpackWillRun reads the same PP rule processJob applies, so the boundary is
// PPUnpack exactly, and an unwired or disabled stage never runs.
func TestQuickCheckStage_UnpackWillRun(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		wired   bool
		enabled bool
		pp      int
		want    bool
	}{
		{"enabled at PPUnpack", true, true, types.PPUnpack, true},
		{"enabled at PPDelete", true, true, types.PPDelete, true},
		{"enabled at PPVerify", true, true, types.PPVerify, false},
		{"disabled", true, false, types.PPDelete, false},
		{"unwired", false, true, types.PPDelete, false},
	} {
		qc, _, up := layoutStages()
		up.SetEnabled(tc.enabled)
		if !tc.wired {
			qc.Unpack = nil
		}
		if got := qc.unpackWillRun(&Job{PP: tc.pp}); got != tc.want {
			t.Errorf("%s: unpackWillRun = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// deferredSets defers nothing when every entry was identified, without
// looking at the directory at all: the job here has no directory.
func TestDeferredSets_FullyAccountedSetIsNot(t *testing.T) {
	t.Parallel()

	qc, _, _ := layoutStages()
	job, _ := stageJob(t)
	job.PP = types.PPDelete
	job.DownloadDir = filepath.Join(t.TempDir(), "absent")
	entry := par2.FileDesc{FileName: "feature.bin", Set: "feature"}
	id := par2.Identification{Files: []par2.Identified{{OnDisk: "feature.bin", Desc: entry}}}
	if got := qc.deferredSets(t.Context(), slog.New(slog.DiscardHandler), job, id); len(got) != 0 {
		t.Errorf("deferredSets = %v for a set that identified every entry, want none", got)
	}
	for _, line := range job.OutputLines {
		if strings.Contains(line, "Archive scan failed") {
			t.Errorf("the directory was scanned for a set that identified a file: %q", line)
		}
	}
}

// A directory the archive scan cannot read shows no archive holding anything.
func TestHeldArchiveMembers_ScanErrorIsNotHeld(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	job.DownloadDir = filepath.Join(dir, "does-not-exist")
	if _, ok := heldArchiveMembers(t.Context(), slog.New(slog.DiscardHandler), job, nil); ok {
		t.Error("an unreadable directory was reported as holding the entries")
	}
}

// An archive whose members cannot be listed shows nothing about them.
func TestHeldArchiveMembers_ListingErrorIsNotHeld(t *testing.T) {
	t.Parallel()

	job, dir := layoutBJob(t, "release.rar")
	if err := os.WriteFile(filepath.Join(dir, "release.rar"), []byte("not a rar archive at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	qc, _, _ := layoutStages()
	runQuickCheck(t, qc, job)
	if job.QuickCheck != QuickCheckDamaged {
		t.Errorf("QuickCheck = %s with an archive that cannot be listed, want damaged", job.QuickCheck)
	}
}

// Unidentified tells repair that par2 has nothing here it could verify, so it
// must not run — and must not leave ParError behind for unpack to read. The
// DirectUnpack shortcut is not what spares it: nothing here extracted.
func TestRepairStage_DeclinesWhenQuickCheckUnidentified(t *testing.T) {
	t.Parallel()

	job, _ := layoutBJob(t, "release.rar")
	job.QuickCheck = QuickCheckUnidentified

	stage := &RepairStage{UseGoPar2: true, Log: slog.New(slog.DiscardHandler)}
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.ParError {
		t.Error("ParError = true; repair ran against a par2 set that describes nothing delivered")
	}
	for _, line := range job.OutputLines {
		if strings.HasPrefix(line, "[par2]") {
			t.Errorf("par2 was run for an unidentified set: %q", line)
		}
	}
}
