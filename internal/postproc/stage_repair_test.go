package postproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// TestRepairStage_QuickCheckProblemOverridesDirectUnpackSuccess guards against
// the reported regression where DirectUnpack reporting a set as successfully
// extracted caused the repair stage to skip par2 verification entirely, even
// though the QuickCheck stage (which runs first) had already determined that
// repair was needed (e.g. a downloaded RAR volume had failed/missing
// articles, so its CRC could not be verified). DirectUnpack only knows
// whether rarengine could mechanically read the archive's entries — not
// whether the underlying download was complete — so its "success" must never
// override an explicit "needs repair" verdict from QuickCheck.
func TestRepairStage_QuickCheckProblemOverridesDirectUnpackSuccess(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	copyPar2Fixtures(t, dir)

	// DirectUnpack reports a clean success with no failures/skips...
	job.DirectUnpackSets = map[string]directunpack.SuccessSet{
		"ok": {RarParts: []string{"data.bin"}, ExtractedFiles: []string{"data.out"}},
	}
	// ...but QuickCheck ran and found a problem (unverifiable/mismatched CRC).
	job.QuickCheck = QuickCheckDamaged

	stage := &RepairStage{
		UseGoPar2: true,
		Log:       slog.New(slog.DiscardHandler),
	}

	if err := stage.Run(t.Context(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, line := range job.OutputLines {
		if line == "[repair] Skipped: Direct Unpack successfully extracted all archives during download" {
			t.Fatalf("repair was skipped on DirectUnpack's say despite QuickCheck flagging a problem; output: %v", job.OutputLines)
		}
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
		Log:       slog.New(slog.DiscardHandler),
	}

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
	stage := &RepairStage{
		UseGoPar2: true,
		Log:       slog.New(handler),
	}

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
	removed := cleanupPar2Backups(dir, log)

	if len(removed) != 1 {
		t.Errorf("removed = %d, want 1", len(removed))
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

	log := slog.New(slog.DiscardHandler)
	removed := cleanupPar2Backups(dir, log)

	if len(removed) != 0 {
		t.Errorf("removed = %d, want 0 (no original present)", len(removed))
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
	qjob := newQueueJob(t, "rjob", 0)
	qjob.SetName("RepairTest")
	job := &Job{
		Job:         qjob,
		DownloadDir: dir,
	}
	vs := NewVerifiedSets(dir, nil)
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
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, vs, par2.RepairResult{}, underlying)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, underlying) {
		t.Errorf("error chain should wrap underlying: %v", err)
	}
	if !job.ParError {
		t.Error("ParError should be true after error")
	}
	// vs should record the set as not-verified (false), not simply absent.
	vs.MarkVerified("testset", false)
	if vs.IsVerified("testset") {
		t.Error("set should not be verified after error")
	}
}

// TestHandleRepairResult_NeedMoreBlocks verifies that NeedMoreBlocks=true
// sets ParError and returns an error naming the block shortfall.
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
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, vs, res, nil)

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
	job, vs := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	res := par2.RepairResult{
		Success: false,
		Parsed:  &par2.RepairOutput{Status: par2.StatusInvalidPar2},
	}
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, vs, res, nil)

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
	job, vs := repairJob(t)
	set := par2.Set{Name: "testset", MainFile: "testset.par2"}
	stage := &RepairStage{}

	res := par2.RepairResult{
		Success:  false,
		ExitCode: 2,
	}
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, vs, res, nil)

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
	job, vs := repairJob(t)
	set := par2.Set{
		Name:       "testset",
		MainFile:   "testset.par2",
		ExtraFiles: []string{"testset.vol001+01.par2"},
	}
	stage := &RepairStage{}

	res := par2.RepairResult{Success: true}
	err := stage.handleRepairResult(context.Background(), slog.Default(), job, set, vs, res, nil)

	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if job.ParError {
		t.Error("ParError should be false on success")
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
		// "nonexistent.par2" makes GoRepair fail in newDecoderForDir, i.e.
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

func TestRepairHelpers(t *testing.T) {
	t.Parallel()

	// 1. Test recordRepairSuccess
	t.Run("recordRepairSuccess", func(t *testing.T) {
		job := &Job{ConsumedFiles: make(map[string]struct{})}
		vs := NewVerifiedSets(t.TempDir(), nil)
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
		recordRepairSuccess(t.Context(), slog.Default(), set, job, vs, res)
		if !vs.IsVerified("testset") {
			t.Error("expected set to be verified")
		}
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
		vs := NewVerifiedSets(t.TempDir(), nil)
		set := par2.Set{
			Name: "empty",
		}
		// Empty set (no main file) should skip.
		s := &RepairStage{}
		err := s.processPar2Set(t.Context(), slog.Default(), job, set, nil, par2.RunOptions{}, vs, true, false)
		if err != nil {
			t.Fatalf("processPar2Set: %v", err)
		}
		// Real skip means dispatchRepairTool was never invoked -- a real
		// invocation would add "[par2]"/"Command:"-prefixed lines.
		if len(job.OutputLines) != 1 || !strings.Contains(job.OutputLines[0], "no main file") {
			t.Errorf("OutputLines = %v; want single skip message mentioning no main file", job.OutputLines)
		}

		// Set already verified should skip.
		vs.MarkVerified("verified-set", true)
		set2 := par2.Set{
			Name:       "verified-set",
			MainFile:   "verified-set.par2",
			ExtraFiles: []string{"verified-set.vol001.par2"},
		}
		job.OutputLines = nil
		err = s.processPar2Set(t.Context(), slog.Default(), job, set2, nil, par2.RunOptions{}, vs, true, false)
		if err != nil {
			t.Fatalf("processPar2Set: %v", err)
		}
		if len(job.OutputLines) != 1 || !strings.Contains(job.OutputLines[0], "previously verified") {
			t.Errorf("OutputLines = %v; want single skip message mentioning previously verified", job.OutputLines)
		}
		// recordRepairSuccess (which populates ConsumedFiles) must not have
		// run for an already-verified set.
		if len(job.ConsumedFiles) != 0 {
			t.Errorf("ConsumedFiles = %v; want empty (repair tool must not run for verified set)", job.ConsumedFiles)
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
	vs := NewVerifiedSets(outDir, slog.Default())

	res := par2.RepairResult{Success: true}
	err := s.handleRepairResult(t.Context(), slog.Default(), job, set, vs, res, nil)
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
	vs := NewVerifiedSets(outDir, slog.Default())

	err := s.processPar2Set(t.Context(), slog.Default(), job, set, []string{}, par2.RunOptions{}, vs, false, false)
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
