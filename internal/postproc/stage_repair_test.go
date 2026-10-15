package postproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/testutil"
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

// A DirectUnpack success says nothing about a file beside the RAR sets it
// extracted, so with quickcheck disabled it must not stand in for par2: a
// damaged file par2 protects beside such a set is repaired, or fails the job,
// and is never passed as clean (#953).
func TestRepairStage_QuickCheckDisabledRepairsBesideDirectUnpackSuccess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		duSet bool
	}{
		{"DirectUnpack extracted a sibling RAR set", true},
		{"no DirectUnpack", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job, dir := stageJob(t)
			copyPar2Fixtures(t, dir)
			payload := filepath.Join(dir, "data.bin")
			orig, err := os.ReadFile(payload)
			if err != nil {
				t.Fatal(err)
			}
			damaged := slices.Clone(orig)
			for i := 1000; i < 1100; i++ {
				damaged[i] ^= 0xFF
			}
			if err := os.WriteFile(payload, damaged, 0o644); err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"Subs.part01.rar", "Subs.part02.rar", "Subs.srt"} {
				if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.duSet {
				job.DirectUnpackSets = map[string]directunpack.SuccessSet{
					"Subs": {
						RarParts:       []string{filepath.Join(dir, "Subs.part01.rar"), filepath.Join(dir, "Subs.part02.rar")},
						ExtractedFiles: []string{filepath.Join(dir, "Subs.srt")},
					},
				}
			}

			qc := NewQuickCheckStage()
			qc.Log = slog.New(slog.DiscardHandler)
			qc.SetEnabled(false)
			if err := qc.Run(t.Context(), job); err != nil {
				t.Fatalf("QuickCheckStage.Run: %v", err)
			}
			if job.quickCheck != QuickCheckNotRun {
				t.Fatalf("fixture guard: quickcheck recorded %s, want %s", job.quickCheck, QuickCheckNotRun)
			}

			repair := appliedRepairStage(RepairConfig{UseGoPar2: true}, slog.New(slog.DiscardHandler))
			// Whether the engine repairs the file or fails the set is not this
			// test's question; either one keeps the damage from shipping.
			runErr := repair.Run(t.Context(), job)

			got, err := os.ReadFile(payload)
			if err != nil {
				t.Fatal(err)
			}
			if intact := slices.Equal(got, orig); !intact && !job.ParError {
				t.Errorf("data.bin is damaged and ParError=false (Run: %v): repair let the damage through; output: %v",
					runErr, job.OutputLines)
			}
		})
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

	stage := appliedRepairStage(RepairConfig{UseGoPar2: true}, slog.New(slog.DiscardHandler))

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if job.ParError {
		t.Errorf("ParError = true after successful GoRepair; want false")
	}
}

// countingHandler is a minimal slog.Handler that counts records by message,
// used to detect duplicate log emission.
type countingHandler struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingHandler() *countingHandler { return &countingHandler{counts: make(map[string]int)} }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[r.Message]++
	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(message string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[message]
}

// TestRepairStage_GoRepairDoesNotDuplicateEngineLogLines guards against a
// reported regression: par2engine's own log records (e.g. "Candidate file(s)
// to consider (1):") were appearing twice in the log — once via go_par2's
// teeHandler forwarding the record to the base handler, and once more because
// dispatchRepairTool's onLine callback additionally called log.Info(line),
// re-emitting the same message as a second, independent record. onLine must
// only feed job.OnOutput; the structured log entry is already produced by the
// teeHandler pass-through.
func TestRepairStage_GoRepairDoesNotDuplicateEngineLogLines(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyPar2Fixtures(t, dir)

	var onOutputLines []string
	job.OnOutput = func(_ string, line string) {
		onOutputLines = append(onOutputLines, line)
	}

	handler := newCountingHandler()
	stage := appliedRepairStage(RepairConfig{UseGoPar2: true}, slog.New(handler))

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	const engineMsg = "Candidate file(s) to consider (1):"
	if got := handler.count(engineMsg); got != 1 {
		t.Errorf("log message %q recorded %d time(s); want exactly 1 (duplicate engine log line)", engineMsg, got)
	}

	// The onLine → job.OnOutput path (UI/history feed) must still fire even
	// though the redundant log.Info call was removed.
	found := false
	for _, l := range onOutputLines {
		if strings.Contains(l, engineMsg) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected job.OnOutput to receive a line containing %q, got: %v", engineMsg, onOutputLines)
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

	log := slog.New(slog.DiscardHandler)
	repaired := map[string]struct{}{"movie.part01.rar": {}}
	removed := cleanupPar2Backups(dir, repaired, log)

	if len(removed) != 1 || removed[0] != "movie.part01.rar.1" {
		t.Errorf("removed = %v, want [movie.part01.rar.1]", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "movie.part01.rar.1")); err != nil {
		t.Errorf("backup file was removed before finalize: %v", err)
	}
}

func TestCleanupPar2Backups_PreservesIfOriginalMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Only the backup exists — no repaired original.
	backupPath := filepath.Join(dir, "movie.part01.rar.1")
	os.WriteFile(backupPath, []byte("damaged-orig"), 0o644) //nolint:errcheck

	log := slog.New(slog.DiscardHandler)
	repaired := map[string]struct{}{"movie.part01.rar": {}}
	removed := cleanupPar2Backups(dir, repaired, log)

	if len(removed) != 0 {
		t.Errorf("removed = %d, want 0 (no original present)", len(removed))
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("backup removed despite missing original: %v", err)
	}
}

func TestCleanupPar2Backups_ScopedToRepairedTargetsAndSkipsDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Directory named release.v1 must not count as an existing repaired file for release.v1.1.
	if err := os.MkdirAll(filepath.Join(dir, "release.v1"), 0o750); err != nil {
		t.Fatalf("MkdirAll release.v1: %v", err)
	}
	for _, name := range []string{
		"disc.iso",           // unrepaired payload
		"disc.iso.1",         // unrepaired payload with .1 suffix — must be kept
		"libfoo.so",          // unrepaired payload
		"libfoo.so.1",        // unrepaired payload — must be kept
		"release.v1.1",       // file next to directory release.v1 — must be kept
		"movie.part01.rar",   // repaired target
		"movie.part01.rar.1", // backup of repaired target — must be queued
		"payload.bin",        // repaired target
		"payload.bin.1",      // backup 1 of repaired target — must be queued
		"payload.bin.2",      // backup 2 of repaired target — must be queued
		"payload.bin.001",    // zero-padded split part, NOT a par2 .1 backup
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	log := slog.New(slog.DiscardHandler)
	repaired := map[string]struct{}{
		"movie.part01.rar": {},
		"payload.bin":      {},
		"release.v1":       {},
	}
	removed := cleanupPar2Backups(dir, repaired, log)
	slices.Sort(removed)
	want := []string{"movie.part01.rar.1", "payload.bin.1", "payload.bin.2"}
	if !slices.Equal(removed, want) {
		t.Errorf("cleanupPar2Backups = %v, want %v", removed, want)
	}
	if got := cleanupPar2Backups(dir, nil, log); len(got) != 0 {
		t.Errorf("cleanupPar2Backups with nil repairedTargets = %v, want empty", got)
	}
	if !isPar2BackupNumber("1") || !isPar2BackupNumber("999") || isPar2BackupNumber("01") || isPar2BackupNumber("1000") || isPar2BackupNumber("1a") || isPar2BackupNumber("") {
		t.Error("isPar2BackupNumber classification failed")
	}

	// Verify recordRepairSuccess records backups of DamagedTargets and Renames
	// onto job.par2Backups, RecoverPar2NamesStage excludes them via
	// par2RenameExclusions, and Par2CleanupStage promotes only those backups.
	job := repairJob(t)
	job.DownloadDir = dir
	recordRepairSuccess(t.Context(), log, par2.Set{Name: "movie", MainFile: filepath.Join(dir, "movie.par2")}, job, par2.RepairResult{
		Success: true,
		Parsed: &par2.RepairOutput{
			DamagedTargets: []string{"movie.part01.rar"},
			Renames:        map[string]string{"payload.bin": "obf.bin"},
		},
	})
	excl := job.par2RenameExclusions()
	for _, wantExcl := range []string{"movie.part01.rar.1", "payload.bin.1", "payload.bin.2"} {
		if _, ok := excl[wantExcl]; !ok {
			t.Errorf("par2RenameExclusions missing %q: %v", wantExcl, excl)
		}
	}
	if _, ok := excl["disc.iso.1"]; ok {
		t.Errorf("unrepaired disc.iso.1 was excluded as a par2 backup: %v", excl)
	}
	if err := NewPar2CleanupStage(true).Run(t.Context(), job); err != nil {
		t.Fatalf("Par2CleanupStage.Run: %v", err)
	}
	for _, wantPending := range []string{"movie.part01.rar.1", "payload.bin.1", "payload.bin.2"} {
		if !job.isPendingDeletion(wantPending) {
			t.Errorf("%q was not promoted to pendingDeletions by Par2CleanupStage", wantPending)
		}
	}
	for _, keep := range []string{"disc.iso.1", "libfoo.so.1", "release.v1.1", "payload.bin.001"} {
		if job.isPendingDeletion(keep) {
			t.Errorf("unrepaired/non-backup %q was queued for deletion", keep)
		}
	}
	if _, err := job.outputFileList(); err == nil {
		t.Error("expected error from outputFileList before finalize runs")
	}
	job.clearPendingDeletions()
	if len(job.pendingDeletions) != 0 {
		t.Errorf("pendingDeletions after clearPendingDeletions = %v, want empty", job.pendingDeletions)
	}
}

// ---------------------------------------------------------------------------
// handleRepairResult — focused unit tests for all 5 outcome branches
// ---------------------------------------------------------------------------

// repairJob creates a minimal Job wired to a real temp dir.
func repairJob(t *testing.T) *Job {
	t.Helper()
	dir := t.TempDir()
	qjob := newQueueJob(t, "rjob", 0)
	qjob.SetName("RepairTest")
	job := &Job{
		Job:         qjob,
		DownloadDir: dir,
	}
	return job
}

// TestHandleRepairResult_ErrorPath verifies that a non-nil err sets ParError
// and returns an error wrapping the original.
func TestHandleRepairResult_ErrorPath(t *testing.T) {
	t.Parallel()
	job := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	underlying := fmt.Errorf("par2 crashed with code 137")
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, par2.RepairResult{}, underlying)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, underlying) {
		t.Errorf("error chain should wrap underlying: %v", err)
	}
	if !job.ParError {
		t.Error("ParError should be true after error")
	}
}

// TestHandleRepairResult_NeedMoreBlocks verifies that NeedMoreBlocks=true
// sets ParError and returns an error naming the block shortfall.
func TestHandleRepairResult_NeedMoreBlocks(t *testing.T) {
	t.Parallel()
	job := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	res := par2.RepairResult{
		Success:        false,
		NeedMoreBlocks: true,
		BlocksNeeded:   42,
	}
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, res, nil)

	if err == nil {
		t.Fatal("expected error for NeedMoreBlocks")
	}
	if !job.ParError {
		t.Error("ParError should be true")
	}
	// Distinguishes this branch from InvalidPar2: only this branch's error
	// names "more recovery blocks".
	if !strings.Contains(err.Error(), "need 42 more recovery blocks") {
		t.Errorf("err.Error() = %q; want it to mention needing more recovery blocks", err.Error())
	}
}

// TestHandleRepairResult_InvalidPar2 verifies that a corrupt/missing par2
// file (Parsed.Status == StatusInvalidPar2) sets ParError and returns an
// error distinct from NeedMoreBlocks.
func TestHandleRepairResult_InvalidPar2(t *testing.T) {
	t.Parallel()
	job := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	res := par2.RepairResult{
		Success: false,
		Parsed:  &par2.RepairOutput{Status: par2.StatusInvalidPar2},
	}
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, res, nil)

	if err == nil {
		t.Fatal("expected error for InvalidPar2")
	}
	if !job.ParError {
		t.Error("ParError should be true")
	}
	// Distinguishes this branch from NeedMoreBlocks: only this branch's error
	// names the main par2 file as corrupt or missing.
	if !strings.Contains(err.Error(), "main par2 file corrupt or missing") {
		t.Errorf("err.Error() = %q; want it to mention the main par2 file being corrupt or missing", err.Error())
	}
}

// TestHandleRepairResult_UnsuccessfulGeneric verifies the catch-all failed
// path: ParError=true and a non-nil error naming the exit code.
func TestHandleRepairResult_UnsuccessfulGeneric(t *testing.T) {
	t.Parallel()
	job := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	res := par2.RepairResult{
		Success:  false,
		ExitCode: 2,
	}
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, res, nil)

	if err == nil {
		t.Fatal("expected error for generic unsuccessful repair")
	}
	if !job.ParError {
		t.Error("ParError should be true")
	}
	// Distinguishes this branch from NeedMoreBlocks/InvalidPar2: only this
	// branch's error names an exit code rather than a specific par2
	// diagnosis.
	if !strings.Contains(err.Error(), "unsuccessful (exit=2)") {
		t.Errorf("err.Error() = %q; want it to name the exit code", err.Error())
	}
}

// TestHandleRepairResult_Success verifies that a successful repair clears
// ParError, marks the set verified, and returns no error.
func TestHandleRepairResult_Success(t *testing.T) {
	t.Parallel()
	job := repairJob(t)
	set := par2.Set{
		Name:       "testset",
		MainFile:   "testset.par2",
		ExtraFiles: []string{"testset.vol001+01.par2"},
	}
	stage := &RepairStage{}

	res := par2.RepairResult{Success: true}
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, res, nil)

	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if job.ParError {
		t.Error("ParError should be false on success")
	}
	// recordRepairSuccess should have added par2 files to ConsumedFiles.
	if _, ok := job.ConsumedFiles["testset.par2"]; !ok {
		t.Error("MainFile should be in ConsumedFiles after success")
	}
	if _, ok := job.ConsumedFiles["testset.vol001+01.par2"]; !ok {
		t.Error("ExtraFile should be in ConsumedFiles after success")
	}
}

// TestShouldFallbackToExternal pins the native→external fallback decision: the
// definitive go_par2 verdict (not enough recovery data) must NOT trigger a
// redundant external par2 scan, while engine errors and unexpected non-success
// results must.
func TestShouldFallbackToExternal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		res  par2.RepairResult
		err  error
		want bool
	}{
		{
			name: "engine error falls back",
			res:  par2.RepairResult{},
			err:  errors.New("go_par2: par2engine panic"),
			want: true,
		},
		{
			name: "success does not fall back",
			res:  par2.RepairResult{Success: true},
			want: false,
		},
		{
			name: "need more blocks is definitive, no fallback",
			res:  par2.RepairResult{Success: false, NeedMoreBlocks: true, BlocksNeeded: 84},
			want: false,
		},
		{
			// A go_par2 decoder/parse failure surfaces as err != nil, not as a
			// StatusInvalidPar2 result — GoRepair never sets res.Parsed — so it
			// must fall back to let the mature external parser try.
			name: "generic non-success falls back",
			res:  par2.RepairResult{Success: false, ExitCode: 2},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldFallbackToExternal(tc.res, tc.err); got != tc.want {
				t.Errorf("shouldFallbackToExternal(%+v, %v) = %v; want %v", tc.res, tc.err, got, tc.want)
			}
		})
	}
}

// TestNativeRepairReason verifies the fallback-reason string prefers the engine
// error, then a specific block count, then a generic exit-code summary.
func TestNativeRepairReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		res  par2.RepairResult
		err  error
		want string
	}{
		{
			name: "engine error wins",
			res:  par2.RepairResult{NeedMoreBlocks: true, BlocksNeeded: 9, ExitCode: 3},
			err:  errors.New("boom"),
			want: "boom",
		},
		{
			name: "block count when no error",
			res:  par2.RepairResult{NeedMoreBlocks: true, BlocksNeeded: 84},
			want: "needs 84 more recovery blocks",
		},
		{
			name: "generic exit code",
			res:  par2.RepairResult{ExitCode: 2},
			want: "unsuccessful (exit=2)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := nativeRepairReason(tc.res, tc.err); got != tc.want {
				t.Errorf("nativeRepairReason(%+v, %v) = %q; want %q", tc.res, tc.err, got, tc.want)
			}
		})
	}
}

// TestDispatchRepairTool_FallbackRunsWhenBinaryPresent pins the binary-presence
// guard: when go_par2 fails and the external par2 binary IS resolvable, the
// external retry must run (signalled by the "retrying with external" OnOutput
// message). A missing (non-resolvable) binary must NOT retry.
func TestDispatchRepairTool_FallbackRunsWhenBinaryPresent(t *testing.T) {
	t.Parallel()

	// Stub "par2" binary: resolvable + executable, exits non-zero so the
	// external run yields a benign failure. We assert on the branch taken,
	// not the result.
	stub := testutil.WriteExecutable(t, filepath.Join(t.TempDir(), "par2stub"), "#!/bin/sh\nexit 1\n")

	run := func(command string) bool {
		var retried bool
		job := &Job{
			Job:         newQueueJob(t, "testjob", 0),
			DownloadDir: t.TempDir(),
			OnOutput: func(_ string, line string) {
				if strings.Contains(line, "retrying with external") {
					retried = true
				}
			},
		}
		// "nonexistent.par2" makes GoRepair fail in newDecoder, i.e.
		// err != nil. Falling back on such a decoder/parse failure is the
		// intended design (only NeedMoreBlocks skips fallback — see
		// shouldFallbackToExternal), so shouldFallbackToExternal returns true
		// and the binary-presence guard under test decides whether to retry.
		_, _ = dispatchRepairTool(
			t.Context(), slog.Default(), job,
			"nonexistent.par2", nil,
			par2.RunOptions{Command: command},
			true, // useGoPar2
			true, // fallback
		)
		return retried
	}

	if !run(stub) {
		t.Error("expected external fallback to run when the par2 binary is resolvable")
	}
	// A binary that cannot be resolved on PATH must not trigger the retry.
	if run(filepath.Join(t.TempDir(), "definitely-missing-par2-binary")) {
		t.Error("expected no fallback when the par2 binary is not resolvable")
	}
}

// dispatchWithStubFallback runs dispatchRepairTool on dir's data.par2 with
// go_par2 enabled and a stub external par2 that exits 1, and reports whether
// the external retry ran.
func dispatchWithStubFallback(t *testing.T, dir string) (par2.RepairResult, bool) {
	t.Helper()
	var retried bool
	job := &Job{
		Job:         newQueueJob(t, "testjob", 0),
		DownloadDir: dir,
		OnOutput: func(_ string, line string) {
			if strings.Contains(line, "retrying with external") {
				retried = true
			}
		},
	}
	res, _ := dispatchRepairTool(
		t.Context(), slog.Default(), job,
		filepath.Join(dir, "data.par2"), []string{filepath.Join(dir, "data.bin")},
		par2.RunOptions{Command: writeStubBinary(t)},
		true, // useGoPar2
		true, // fallback
	)
	return res, retried
}

// TestDispatchRepairTool_CorruptFileWithBlocksElsewhere_RepairedNatively pins
// that a damaged data.bin whose every block exists in an intact copy under
// another name is rebuilt by go_par2 itself, with no external par2 retry.
func TestDispatchRepairTool_CorruptFileWithBlocksElsewhere_RepairedNatively(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyPar2Fixtures(t, dir)
	intact, err := os.ReadFile(par2Fixture("data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a8f3c0d2e1"), intact, 0o600); err != nil {
		t.Fatal(err)
	}
	damaged := bytes.Clone(intact)
	damaged[len(damaged)/2] ^= 0xFF
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), damaged, 0o600); err != nil {
		t.Fatal(err)
	}

	res, retried := dispatchWithStubFallback(t, dir)
	if retried {
		t.Error("the external par2 retry ran; go_par2 should have rebuilt data.bin itself")
	}
	if !res.Success {
		t.Errorf("Success = false; want go_par2's native rebuild to succeed\nOutput: %s", res.Output)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "data.bin")); err != nil || !bytes.Equal(got, intact) {
		t.Errorf("data.bin does not hold its original bytes (err=%v)", err)
	}
}

// TestDispatchRepairTool_SymlinkAtProtectedName_FallsBack pins the
// stage-level effect of go_par2's intactness verdict on a set the engine
// itself calls clean: par2engine follows a symlink at data.bin to an intact
// copy, gonzbd's check requires a regular file there, and the refusal must
// reach the external par2 retry instead of being filed as repaired.
func TestDispatchRepairTool_SymlinkAtProtectedName_FallsBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyPar2Fixtures(t, dir)
	if err := os.Rename(filepath.Join(dir, "data.bin"), filepath.Join(dir, "payload")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("payload", filepath.Join(dir, "data.bin")); err != nil {
		t.Fatal(err)
	}

	res, retried := dispatchWithStubFallback(t, dir)
	if !retried {
		t.Error("go_par2 accepted a symlink at data.bin; the external par2 retry did not run")
	}
	if res.Success {
		t.Errorf("Success = true; the stub external par2 exits 1\nOutput: %s", res.Output)
	}
}

func TestRepairHelpers(t *testing.T) {
	t.Parallel()

	// 1. Test recordRepairSuccess
	t.Run("recordRepairSuccess", func(t *testing.T) {
		job := &Job{ConsumedFiles: make(map[string]struct{})}
		set := par2.Set{
			Name:       "testset",
			MainFile:   "testset.par2",
			ExtraFiles: []string{"testset.vol001.par2"},
		}
		res := par2.RepairResult{
			Parsed: &par2.RepairOutput{
				Renames:       map[string]string{"repaired.txt": "obfuscated.txt"},
				UsedJoinables: []string{"joinable.txt"},
			},
		}
		job.DownloadDir = t.TempDir()
		recordRepairSuccess(t.Context(), slog.Default(), set, job, res)
		if _, ok := job.ConsumedFiles["testset.par2"]; !ok {
			t.Error("expected testset.par2 to be consumed")
		}
		if job.Par2Renames["repaired.txt"] != "obfuscated.txt" {
			t.Error("expected par2 rename to be recorded")
		}
		if _, ok := job.ConsumedFiles[filepath.Join(job.DownloadDir, "joinable.txt")]; !ok {
			t.Error("expected joinable.txt to be consumed")
		}
	})

	// 2. Test dispatchRepairTool
	t.Run("dispatchRepairTool", func(t *testing.T) {
		job := &Job{
			Job:         newQueueJob(t, "testjob", 0),
			DownloadDir: t.TempDir(),
		}

		// Scenario A: Native Go, no fallback
		resA, errA := dispatchRepairTool(
			t.Context(),
			slog.Default(),
			job,
			"nonexistent.par2",
			nil,
			par2.RunOptions{},
			true,  // useGoPar2
			false, // fallback
		)
		if errA == nil {
			t.Error("scenario A: expected native decoder error for nonexistent.par2")
		}
		if resA.CommandLine != "" {
			t.Errorf("scenario A: CommandLine = %q, want empty (no external command should have run)", resA.CommandLine)
		}

		// Scenarios B and C exercise the external par2 path. Use a stub
		// binary (matching TestDispatchRepairTool_FallbackRunsWhenBinaryPresent's
		// pattern below) rather than depending on a real "par2" in PATH --
		// this repo's CI doesn't install par2 for the plain `go test ./...`
		// job, only for `-tags=integration`.
		// Scenario B: External only
		resB, errB := dispatchRepairTool(
			t.Context(),
			slog.Default(),
			job,
			"nonexistent.par2",
			nil,
			par2.RunOptions{Command: writeStubBinary(t)},
			false, // useGoPar2
			false, // fallback
		)
		if errB != nil {
			t.Errorf("scenario B: expected external tool to return a result (not a decoder error), got err=%v", errB)
		}
		if resB.CommandLine == "" {
			t.Error("scenario B: expected external command to have run")
		}

		// Scenario C: Native Go with external fallback -- should behave like
		// B (fallback ran the external tool), proving the fallback path
		// actually executed rather than silently returning A's failure.
		resC, errC := dispatchRepairTool(
			t.Context(),
			slog.Default(),
			job,
			"nonexistent.par2",
			nil,
			par2.RunOptions{Command: writeStubBinary(t)},
			true, // useGoPar2
			true, // fallback
		)
		if errC != nil {
			t.Errorf("scenario C: expected fallback to succeed in returning a result, got err=%v", errC)
		}
		if resC.CommandLine == "" {
			t.Error("scenario C: expected external fallback command to have run")
		}
	})

	// 3. Test processPar2Set
	t.Run("processPar2Set", func(t *testing.T) {
		job := &Job{
			Job:           newQueueJob(t, "testjob", 0),
			ConsumedFiles: make(map[string]struct{}),
			DownloadDir:   t.TempDir(),
		}
		set := par2.Set{
			Name: "empty",
		}
		// Empty set (no main file) should skip.
		s := &RepairStage{}
		err := s.processPar2Set(t.Context(), slog.Default(), job, set, nil, par2.RunOptions{}, true, false)
		if err != nil {
			t.Fatalf("processPar2Set: %v", err)
		}
		// Real skip means dispatchRepairTool was never invoked -- a real
		// invocation would add "[par2]"/"Command:"-prefixed lines.
		if len(job.OutputLines) != 1 || !strings.Contains(job.OutputLines[0], "no main file") {
			t.Errorf("OutputLines = %v; want single skip message mentioning no main file", job.OutputLines)
		}

	})
}

func TestRepairStage_ContainmentViolation(t *testing.T) {
	t.Parallel()

	job, outDir := stageJob(t)
	tmpDir := filepath.Dir(outDir)

	// Create a symlink in outDir pointing outside outDir.
	outsideFile := filepath.Join(tmpDir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	symlink := filepath.Join(outDir, "bad_link.txt")
	if err := os.Symlink(outsideFile, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	s := NewRepairStage()
	set := par2.Set{
		Name:       "bad_set",
		MainFile:   "bad_set.par2",
		ExtraFiles: []string{"bad_set.vol001.par2"},
	}

	res := par2.RepairResult{Success: true}
	err := s.handleRepairResult(t.Context(), slog.Default(), job, set, res, nil)
	if err == nil {
		t.Fatalf("expected containment violation error, got nil")
	}
	if !strings.Contains(err.Error(), "containment check") {
		t.Errorf("err = %v; want containment check error", err)
	}
	if !job.ParError {
		t.Errorf("job.ParError = false; want true on containment violation")
	}
}

func TestRepairStage_PreRepairContainmentViolation(t *testing.T) {
	t.Parallel()

	job, outDir := stageJob(t)
	tmpDir := filepath.Dir(outDir)

	outsideFile := filepath.Join(tmpDir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	symlink := filepath.Join(outDir, "bad_link.txt")
	if err := os.Symlink(outsideFile, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Place a par2 main file so processPar2Set reaches pre-repair containment check.
	mainPar := filepath.Join(outDir, "test.par2")
	if err := os.WriteFile(mainPar, []byte("par2"), 0o644); err != nil {
		t.Fatalf("write main par2: %v", err)
	}

	s := NewRepairStage()
	set := par2.Set{
		Name:     "bad_set",
		MainFile: "test.par2",
	}

	err := s.processPar2Set(t.Context(), slog.Default(), job, set, []string{}, par2.RunOptions{}, false, false)
	if err == nil {
		t.Fatalf("expected pre-repair containment violation error, got nil")
	}
	if !strings.Contains(err.Error(), "pre-repair containment check") {
		t.Errorf("err = %v; want pre-repair containment check error", err)
	}
	if !job.ParError {
		t.Errorf("job.ParError = false; want true on pre-repair containment violation")
	}
}
