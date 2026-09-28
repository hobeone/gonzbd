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

	"github.com/hobeone/gonzbd/internal/unpack"
)

// layoutBFixture is a Layout B post: release.rar is the only delivered
// payload, and the par2 set (feature.par2 + one recovery volume) was created
// over feature.bin, the file the archive extracts to. No delivered file is
// anything the par2 set names.
func layoutBFixture(name string) string {
	return filepath.Join("..", "..", "test", "fixtures", "par2", "layout_b", name)
}

// layoutBJob copies the Layout B fixture into a fresh download directory,
// with the payload delivered under payloadName, and returns a job whose
// manifest lists that payload with its correct assembled CRC.
func layoutBJob(t *testing.T, payloadName string) (*Job, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"feature.par2", "feature.vol0+1.par2"} {
		copyToDir(t, layoutBFixture(name), dir)
	}
	payload, err := os.ReadFile(layoutBFixture("release.rar"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, payloadName), payload, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	j := buildQCJob(t, "layout-b", payloadName, int64(len(payload)), crc32.ChecksumIEEE(payload))
	return &Job{Job: j, DownloadDir: dir}, dir
}

// A Layout B post is healthy and extractable, and the pipeline must extract
// it. Its par2 set describes files that do not exist until unpack has run, so
// a repair attempted before unpack fails on this compressed archive — and a
// failed repair skips unpack unconditionally. The archive's own checksums are what verify its
// extraction here, so repair has to decline rather than fail.
func TestLayoutB_ArchiveExtractsDespitePar2NamingItsContents(t *testing.T) {
	t.Parallel()

	job, dir := layoutBJob(t, "release.rar")

	qc := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	qc.SetEnabled(true)
	repair := &RepairStage{UseGoPar2: true, Log: slog.New(slog.DiscardHandler)}
	up := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, false)
	up.SetEnabled(true)

	// The runner records a stage error and carries on, so these do too.
	var stageErrs []string
	for _, s := range []Stage{qc, repair, up} {
		if err := s.Run(t.Context(), job); err != nil {
			stageErrs = append(stageErrs, s.Name()+": "+err.Error())
		}
	}

	if job.ParError {
		t.Fatalf("ParError = true after a repair of files that do not exist yet (QuickCheck=%s), so unpack was skipped; stage errors: %v",
			job.QuickCheck, stageErrs)
	}
	if len(stageErrs) > 0 {
		t.Errorf("stage errors: %v", stageErrs)
	}
	if job.UnpackError {
		t.Fatal("UnpackError = true; want a clean extraction")
	}
	got, err := os.ReadFile(filepath.Join(dir, "feature.bin"))
	if err != nil {
		t.Fatalf("the archive was not extracted: %v", err)
	}
	wantLine, err := os.ReadFile(layoutBFixture("feature.bin.sha256"))
	if err != nil {
		t.Fatalf("read fixture checksum: %v", err)
	}
	sum := sha256.Sum256(got)
	if want := strings.Fields(string(wantLine))[0]; hex.EncodeToString(sum[:]) != want {
		t.Errorf("extracted feature.bin sha256 = %x, want %s", sum, want)
	}
}

// The classification is only defensible where something else verifies the
// content par2 cannot reach. A RAR or 7z archive records a checksum per entry
// that extraction checks; a file join and a tar do not, and a payload that is
// not an archive at all cannot be Layout B — it is the other case the same
// signature describes, an obfuscated file damaged inside its first 16 KB.
// Each of those must stay Damaged, so repair still runs.
func TestQuickCheckStage_NothingIdentifiedNeedsASelfVerifyingArchive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		payload string
		want    QuickCheckOutcome
	}{
		{"release.rar", QuickCheckUnidentified},
		{"release.7z", QuickCheckUnidentified},
		{"release.bin", QuickCheckDamaged},
		{"release.001", QuickCheckDamaged},
		{"release.tar", QuickCheckDamaged},
	}
	for _, tc := range cases {
		t.Run(tc.payload, func(t *testing.T) {
			t.Parallel()

			job, _ := layoutBJob(t, tc.payload)
			qc := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
			qc.SetEnabled(true)
			if err := qc.Run(t.Context(), job); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if job.QuickCheck != tc.want {
				t.Errorf("QuickCheck = %s for a %s payload par2 does not describe, want %s",
					job.QuickCheck, tc.payload, tc.want)
			}
		})
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
	qc := &QuickCheckStage{Log: slog.New(slog.DiscardHandler)}
	qc.SetEnabled(true)
	if err := qc.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.QuickCheck != QuickCheckDamaged {
		t.Errorf("QuickCheck = %s with a par2 entry identified in the directory, want damaged", job.QuickCheck)
	}
}

// A directory the archive scan cannot read proves no archive is there to
// verify anything, so it must not count as one.
func TestHasSelfVerifyingArchive_ScanErrorIsNoArchive(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	job.DownloadDir = filepath.Join(dir, "does-not-exist")
	if hasSelfVerifyingArchive(t.Context(), slog.New(slog.DiscardHandler), job) {
		t.Error("an unreadable directory was reported as holding a self-verifying archive")
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
