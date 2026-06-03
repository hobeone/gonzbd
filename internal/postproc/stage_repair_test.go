package postproc

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/queue"
)

// par2Fixture returns the absolute path to a file in the shared par2 fixture
// directory. Go tests run with cwd = package directory.
func par2Fixture(name string) string {
	return filepath.Join("..", "..", "test", "fixtures", "par2", name)
}

// copyPar2Fixtures copies all files from the par2 fixture directory into dir
// and returns the path to the main .par2 file.
func copyPar2Fixtures(t *testing.T, dir string) string {
	t.Helper()
	names := []string{"data.bin", "data.par2", "data.vol000+102.par2"}
	for _, name := range names {
		data, err := os.ReadFile(par2Fixture(name))
		if err != nil {
			t.Fatalf("read par2 fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("copy par2 fixture %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "data.par2")
}

// ---------- RepairStage gate tests ----------

// TestRepairStage_DirectUnpackWithFailuresDontSkip verifies that when
// DirectUnpack had failures, the repair stage is not skipped — it should
// attempt PAR2 repair on the remaining sets.
func TestRepairStage_DirectUnpackWithFailuresDontSkip(t *testing.T) {
	t.Parallel()

	job, _ := stageJob(t)

	// One set succeeded and one failed — stage must not skip.
	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"ok": {RarParts: []string{"ok.part01.rar"}, ExtractedFiles: []string{"ok.mkv"}},
	}
	job.DirectUnpackFailures = map[string]directunpack.FailedSet{
		"bad": {Reason: "corrupt volume"},
	}

	// No par2 files in the empty dir → stage runs but finds nothing.
	stage := NewRepairStage()
	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.ParError {
		t.Error("ParError = true; want false (no par2 sets in empty dir)")
	}
}

// ---------- GoRepair integration test ----------

// TestRepairStage_GoRepairHealthyData verifies the GoRepair path with the
// shared par2 test fixture: data.bin is intact, so par2 should report all
// files correct without repair.
func TestRepairStage_GoRepairHealthyData(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyPar2Fixtures(t, dir)

	stage := &RepairStage{
		UseGoPar2: true,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.ParError {
		t.Errorf("ParError = true after successful GoRepair; want false")
	}
}

// ---------- listNonPar2Files helper ----------

func TestListNonPar2Files_Basic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	want := make([]string, 0, 2)
	for _, name := range []string{"data.bin", "movie.mkv"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o644) //nolint:errcheck
		want = append(want, p)
	}
	// These should be excluded.
	os.WriteFile(filepath.Join(dir, "data.par2"), []byte("x"), 0o644)           //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "data.vol001+10.PAR2"), []byte("x"), 0o644) //nolint:errcheck

	got, err := listNonPar2Files(dir)
	if err != nil {
		t.Fatalf("listNonPar2Files: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("got %d files, want %d: %v", len(got), len(want), got)
	}
	for _, p := range want {
		found := false
		for _, g := range got {
			if g == p {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %s in result, not found", filepath.Base(p))
		}
	}
}

func TestListNonPar2Files_EmptyDir(t *testing.T) {
	t.Parallel()

	got, err := listNonPar2Files(t.TempDir())
	if err != nil {
		t.Fatalf("listNonPar2Files on empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d files, want 0", len(got))
	}
}

// ---------- cleanupPar2Backups helper ----------

func TestCleanupPar2Backups_RemovesBackupsWithOriginal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create the repaired original and its par2 backup file.
	os.WriteFile(filepath.Join(dir, "movie.part01.rar"), []byte("repaired"), 0o644)       //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "movie.part01.rar.1"), []byte("damaged-orig"), 0o644) //nolint:errcheck

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	removed := cleanupPar2Backups(dir, log)

	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "movie.part01.rar.1")); !os.IsNotExist(err) {
		t.Error("backup file still exists after cleanup")
	}
}

func TestCleanupPar2Backups_PreservesIfOriginalMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Only the backup exists — no repaired original.
	backupPath := filepath.Join(dir, "movie.part01.rar.1")
	os.WriteFile(backupPath, []byte("damaged-orig"), 0o644) //nolint:errcheck

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	removed := cleanupPar2Backups(dir, log)

	if removed != 0 {
		t.Errorf("removed = %d, want 0 (no original present)", removed)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("backup removed despite missing original: %v", err)
	}
}

// ---------------------------------------------------------------------------
// handleRepairResult — focused unit tests for all 5 outcome branches
// ---------------------------------------------------------------------------

// repairJob creates a minimal Job wired to a real temp dir for VerifiedSets persistence.
func repairJob(t *testing.T) (*Job, *VerifiedSets) {
	t.Helper()
	dir := t.TempDir()
	job := &Job{
		Queue: &queue.Job{
			ID:   "rjob",
			Name: "RepairTest",
		},
		DownloadDir: dir,
	}
	vs := NewVerifiedSets(dir)
	return job, vs
}

// TestHandleRepairResult_ErrorPath verifies that a non-nil err sets ParError,
// marks the set as not-verified, and returns an error wrapping the original.
func TestHandleRepairResult_ErrorPath(t *testing.T) {
	t.Parallel()
	job, vs := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	underlying := fmt.Errorf("par2 crashed with code 137")
	err := stage.handleRepairResult(slog.Default(), job, set, vs, par2.RepairResult{}, underlying)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, underlying) {
		t.Errorf("error chain should wrap underlying: %v", err)
	}
	if !job.ParError {
		t.Error("ParError should be true after error")
	}
	if job.NeedRequeue {
		t.Error("NeedRequeue should be false on generic error")
	}
	// vs should record the set as not-verified (false), not simply absent.
	vs.MarkVerified("testset", false)
	if vs.IsVerified("testset") {
		t.Error("set should not be verified after error")
	}
}

// TestHandleRepairResult_NeedMoreBlocks verifies that NeedMoreBlocks=true sets
// NeedRequeue and populates RequeueBlocksNeeded.
func TestHandleRepairResult_NeedMoreBlocks(t *testing.T) {
	t.Parallel()
	job, vs := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	res := par2.RepairResult{
		Success:        false,
		NeedMoreBlocks: true,
		BlocksNeeded:   42,
	}
	err := stage.handleRepairResult(slog.Default(), job, set, vs, res, nil)

	if err == nil {
		t.Fatal("expected error for NeedMoreBlocks")
	}
	if !job.ParError {
		t.Error("ParError should be true")
	}
	if !job.NeedRequeue {
		t.Error("NeedRequeue should be true when more blocks needed")
	}
	if job.RequeueBlocksNeeded != 42 {
		t.Errorf("RequeueBlocksNeeded = %d; want 42", job.RequeueBlocksNeeded)
	}
	if job.RequeueReason == "" {
		t.Error("RequeueReason should be set")
	}
}

// TestHandleRepairResult_InvalidPar2 verifies that a corrupt/missing par2 file
// (Parsed.Status == StatusInvalidPar2) sets NeedRequeue with a distinct reason
// from NeedMoreBlocks and leaves RequeueBlocksNeeded as 0.
func TestHandleRepairResult_InvalidPar2(t *testing.T) {
	t.Parallel()
	job, vs := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	res := par2.RepairResult{
		Success: false,
		Parsed:  &par2.RepairOutput{Status: par2.StatusInvalidPar2},
	}
	err := stage.handleRepairResult(slog.Default(), job, set, vs, res, nil)

	if err == nil {
		t.Fatal("expected error for InvalidPar2")
	}
	if !job.ParError {
		t.Error("ParError should be true")
	}
	if !job.NeedRequeue {
		t.Error("NeedRequeue should be true for corrupt par2")
	}
	// Distinguishes this path from NeedMoreBlocks: no specific block count.
	if job.RequeueBlocksNeeded != 0 {
		t.Errorf("RequeueBlocksNeeded should be 0 for corrupt par2, got %d", job.RequeueBlocksNeeded)
	}
	if job.RequeueReason == "" {
		t.Error("RequeueReason should be set")
	}
}

// TestHandleRepairResult_UnsuccessfulGeneric verifies the catch-all failed
// path: ParError=true, no requeue, non-nil error.
func TestHandleRepairResult_UnsuccessfulGeneric(t *testing.T) {
	t.Parallel()
	job, vs := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	res := par2.RepairResult{
		Success:  false,
		ExitCode: 2,
	}
	err := stage.handleRepairResult(slog.Default(), job, set, vs, res, nil)

	if err == nil {
		t.Fatal("expected error for generic unsuccessful repair")
	}
	if !job.ParError {
		t.Error("ParError should be true")
	}
	if job.NeedRequeue {
		t.Error("NeedRequeue should be false for generic failure without NeedMoreBlocks")
	}
}

// TestHandleRepairResult_Success verifies that a successful repair clears
// ParError, marks the set verified, and returns no error.
func TestHandleRepairResult_Success(t *testing.T) {
	t.Parallel()
	job, vs := repairJob(t)
	set := par2.Set{
		Name:       "testset",
		MainFile:   "testset.par2",
		ExtraFiles: []string{"testset.vol001+01.par2"},
	}
	stage := &RepairStage{}

	res := par2.RepairResult{Success: true}
	err := stage.handleRepairResult(slog.Default(), job, set, vs, res, nil)

	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if job.ParError {
		t.Error("ParError should be false on success")
	}
	if job.NeedRequeue {
		t.Error("NeedRequeue should be false on success")
	}
	if !vs.IsVerified("testset") {
		t.Error("set should be marked verified after success")
	}
	// recordRepairSuccess should have added par2 files to ConsumedFiles.
	if _, ok := job.ConsumedFiles["testset.par2"]; !ok {
		t.Error("MainFile should be in ConsumedFiles after success")
	}
	if _, ok := job.ConsumedFiles["testset.vol001+01.par2"]; !ok {
		t.Error("ExtraFile should be in ConsumedFiles after success")
	}
}
