package postproc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/types"
	"github.com/hobeone/gonzbd/internal/unpack"
	"github.com/hobeone/gonzbd/internal/unwanted"
)

// abortBeforeFinalizeStage cancels the job context and returns an error to
// simulate a process crash or stage abort after unpack/par2_cleanup and before
// FinalizeStage runs (#768).
type abortBeforeFinalizeStage struct {
	cancel context.CancelFunc
}

func (abortBeforeFinalizeStage) Name() string { return "abort_before_finalize" }

func (s abortBeforeFinalizeStage) Run(_ context.Context, _ *Job) error {
	if s.cancel != nil {
		s.cancel()
	}
	return errors.New("simulated crash before finalize")
}

type hookHandler struct {
	slog.Handler
	onRecord func(msg string)
}

func (h *hookHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.onRecord != nil {
		h.onRecord(r.Message)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *hookHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &hookHandler{Handler: h.Handler.WithAttrs(attrs), onRecord: h.onRecord}
}

// intactLayoutARar returns the intact Real.Name.rar bytes that
// test/fixtures/par2/layout_a/Real.Name.par2 was created over (reconstructed
// from d8f7a6.rar + Real.Name.vol0+2.par2).
func intactLayoutARar(t *testing.T) string {
	t.Helper()
	job, dir := layoutJob(t, "layout_a", []string{"Real.Name.par2", "Real.Name.vol0+2.par2"},
		par2LayoutFixture("layout_a", "d8f7a6.rar"), "d8f7a6.rar")
	qc, repair, _ := layoutStages()
	runStages(t, job, qc, repair)
	repairedPath := filepath.Join(dir, "Real.Name.rar")
	if _, err := os.Stat(repairedPath); err != nil {
		t.Fatalf("fixture guard: repair did not reconstruct Real.Name.rar: %v", err)
	}
	return repairedPath
}

// buildPipelineForDeferredCleanup constructs a pipeline with quickcheck,
// repair, unpack, par2_cleanup, deobfuscate, unwanted_cleanup,
// extension_cleanup, and finalize.
func buildPipelineForDeferredCleanup(unpackCleanup, par2Cleanup bool, extraAfterUnpack ...Stage) []Stage {
	discard := slog.New(slog.DiscardHandler)
	up := NewUnpackStageWith(unpack.Options{UseGoRAR: true, UseGo7z: true}, unpackCleanup)
	up.SetEnabled(true)
	up.Log = discard

	qc := &QuickCheckStage{Log: discard, Unpack: up}
	qc.SetEnabled(true)

	repair := &RepairStage{UseGoPar2: true, Log: discard}

	par2Clean := NewPar2CleanupStage(par2Cleanup)
	par2Clean.Log = discard

	deob := NewDeobfuscateStage()
	deob.SetEnabled(true)
	deob.Log = discard

	unwantedStage := NewUnwantedCleanupStage(func() (unwanted.Rules, error) {
		return unwanted.NewRules(unwanted.ActionFail, unwanted.ModeBlacklist, []string{"rar", "par2", "1"})
	})
	unwantedStage.Log = discard

	extClean := NewExtensionCleanupStage([]string{"rar", "par2", "1", "sfv"})
	extClean.Log = discard

	finalize := NewFinalizeStage()
	finalize.Log = discard

	stages := make([]Stage, 0, 8+len(extraAfterUnpack))
	stages = append(stages, qc, repair, up)
	stages = append(stages, extraAfterUnpack...)
	stages = append(stages, par2Clean, deob, unwantedStage, extClean, finalize)
	return stages
}

// TestDeferredCleanup_RerunAfterCrashBeforeFinalize verifies #768:
//  1. Build a download directory with a RAR archive and a par2 set that
//     protects the archive volume.
//  2. Run the pipeline up to and including unpack, then stop before finalize
//     by injecting a stage that cancels and returns an error.
//  3. Run the full pipeline again on the same download directory.
//  4. Assert the rerun completes with ParError == false, QuickCheck ==
//     QuickCheckClean (not Damaged), FailMsg == "", and the extracted file
//     present in FinalDir while the archive and par2 files are removed from
//     FinalDir after finalize succeeds.
func TestDeferredCleanup_RerunAfterCrashBeforeFinalize(t *testing.T) {
	t.Parallel()

	intactRar := intactLayoutARar(t)
	job1, dir := layoutJob(t, "layout_a", []string{"Real.Name.par2", "Real.Name.vol0+2.par2"},
		intactRar, "Real.Name.rar")
	job1.Job.SetName("Real.Name")
	finalDir := filepath.Join(t.TempDir(), "complete", "Real.Name")
	job1.FinalDir = finalDir

	ctx1, cancel1 := context.WithCancel(t.Context())
	defer cancel1()

	pp1 := New(Options{
		Stages: buildPipelineForDeferredCleanup(true, true, abortBeforeFinalizeStage{cancel: cancel1}),
		Logger: slog.New(slog.DiscardHandler),
	})
	pp1.processJob(ctx1, job1)

	// The archive and par2 files must still be in DownloadDir because finalize
	// never ran.
	for _, name := range []string{"Real.Name.rar", "Real.Name.par2", "Real.Name.vol0+2.par2"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("after aborted first run, %s was deleted before finalize: %v", name, err)
		}
	}

	// Second run on the same DownloadDir (simulating restart recovery).
	job2 := &Job{
		Job:         job1.Job,
		Filename:    job1.Filename,
		DownloadDir: dir,
		FinalDir:    finalDir,
		PP:          types.PPDelete,
	}
	pp2 := New(Options{
		Stages: buildPipelineForDeferredCleanup(true, true),
		Logger: slog.New(slog.DiscardHandler),
	})
	pp2.processJob(t.Context(), job2)

	if job2.QuickCheck == QuickCheckDamaged {
		t.Errorf("rerun QuickCheck = %s, want not Damaged (archives must survive until finalize)", job2.QuickCheck)
	}
	if job2.ParError {
		t.Errorf("rerun ParError = true, want false")
	}
	if job2.UnpackError {
		t.Errorf("rerun UnpackError = true, want false")
	}
	if job2.FailMsg != "" {
		t.Errorf("rerun FailMsg = %q, want empty", job2.FailMsg)
	}

	assertFeatureExtracted(t, finalDir)
	assertAbsent(t, finalDir, "Real.Name.rar", "Real.Name.par2", "Real.Name.vol0+2.par2", pendingDeletionsFile)

	t.Run("leftover .gonzbd-tmp-unpack-* directory swept even when unpack is skipped", func(t *testing.T) {
		t.Parallel()

		job, dir := stageJob(t)
		job.PP = types.PPVerify // PP=1 skips UnpackStage
		job.Job.SetName("Skipped.Unpack")
		subFinalDir := filepath.Join(t.TempDir(), "complete", "Skipped.Unpack")
		job.FinalDir = subFinalDir

		if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("payload"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}
		staleStageDir := filepath.Join(dir, ".gonzbd-tmp-unpack-stale")
		if err := os.MkdirAll(staleStageDir, 0o750); err != nil {
			t.Fatalf("mkdir staleStageDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(staleStageDir, "partial.mkv"), []byte("truncated"), 0o600); err != nil {
			t.Fatalf("write partial.mkv: %v", err)
		}

		discard := slog.New(slog.DiscardHandler)
		up := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true)
		up.SetEnabled(true)
		up.Log = discard
		finalize := NewFinalizeStage()
		finalize.Log = discard

		pp := New(Options{
			Stages: []Stage{up, finalize},
			Logger: discard,
		})
		pp.processJob(t.Context(), job)

		if job.FailMsg != "" {
			t.Fatalf("FailMsg = %q, want empty", job.FailMsg)
		}
		assertPresent(t, subFinalDir, "movie.mkv")
		assertAbsent(t, subFinalDir, ".gonzbd-tmp-unpack-stale")
	})

	t.Run("recovery after crash between move to _UNPACK_ and prefix strip", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root bypasses read-only directory permissions")
		}

		baseDir := t.TempDir()

		// 1. An untrusted .gonzbd-pending-deletions file in DownloadDir when
		// PendingDeletions is empty must be removed and never delete payload.
		untrustedJob, untrustedDir := stageJob(t)
		untrustedFinal := filepath.Join(baseDir, "complete", "Untrusted.Sidecar")
		untrustedJob.FinalDir = untrustedFinal
		if err := os.WriteFile(filepath.Join(untrustedDir, "movie.mkv"), []byte("delivered"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}
		if err := os.WriteFile(filepath.Join(untrustedDir, pendingDeletionsFile), []byte(`["movie.mkv"]`), 0o600); err != nil {
			t.Fatalf("write untrusted sidecar: %v", err)
		}
		fUntrusted := NewFinalizeStage()
		fUntrusted.Log = slog.New(slog.DiscardHandler)
		if err := fUntrusted.Run(t.Context(), untrustedJob); err != nil {
			t.Fatalf("fUntrusted.Run: %v", err)
		}
		assertPresent(t, untrustedFinal, "movie.mkv")
		assertAbsent(t, untrustedFinal, pendingDeletionsFile)

		// 2. When a single pending file fails to unlink (read-only parent sub),
		// deletePending still removes .gonzbd-pending-deletions so the sidecar
		// is never published into FinalDir.
		partialJob, partialDir := stageJob(t)
		partialFinal := filepath.Join(baseDir, "complete", "Partial.Delete")
		partialJob.FinalDir = partialFinal
		partialJob.PendingDeletions = []string{"part1.rar", filepath.Join("sub", "part2.rar")}
		if err := os.MkdirAll(filepath.Join(partialDir, "sub"), 0o750); err != nil {
			t.Fatalf("mkdir partial sub: %v", err)
		}
		for name, content := range map[string]string{
			"movie.mkv":                       "delivered",
			"part1.rar":                       "archive-1",
			filepath.Join("sub", "part2.rar"): "archive-2",
		} {
			if err := os.WriteFile(filepath.Join(partialDir, name), []byte(content), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		if err := os.Chmod(filepath.Join(partialDir, "sub"), 0o500); err != nil {
			t.Fatalf("chmod partial sub: %v", err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(filepath.Join(partialDir, "sub"), 0o750)
			_ = os.Chmod(filepath.Join(partialFinal, "sub"), 0o750)
		})
		if err := fUntrusted.Run(t.Context(), partialJob); err != nil {
			t.Fatalf("fUntrusted.Run(partialJob): %v", err)
		}
		assertPresent(t, partialFinal, "movie.mkv", filepath.Join("sub", "part2.rar"))
		assertAbsent(t, partialFinal, "part1.rar", pendingDeletionsFile)

		// 3. When WriteAtomicBytesPerm fails (e.g. pendingDeletionsFile is
		// squatted by a non-empty directory), moveToDest logs a warning.
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))
		finalize := NewFinalizeStage()
		finalize.SetFolderRename(true)
		finalize.Log = logger

		warnJob, warnDir := stageJob(t)
		warnFinal := filepath.Join(baseDir, "complete", "Warn.Sidecar")
		warnJob.FinalDir = warnFinal
		warnJob.PendingDeletions = []string{"part1.rar"}
		if err := os.WriteFile(filepath.Join(warnDir, "movie.mkv"), []byte("delivered"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}
		if err := os.WriteFile(filepath.Join(warnDir, "part1.rar"), []byte("rar"), 0o600); err != nil {
			t.Fatalf("write part1.rar: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(warnDir, pendingDeletionsFile, "child"), 0o750); err != nil {
			t.Fatalf("mkdir squatted sidecar dir: %v", err)
		}
		if err := finalize.Run(t.Context(), warnJob); err != nil {
			t.Fatalf("finalize.Run(warnJob): %v", err)
		}
		if !strings.Contains(logBuf.String(), "Failed to write pending deletions sidecar:") {
			t.Errorf("expected sidecar write warning in log, got %q", logBuf.String())
		}

		// 4. Calling FinalizeStage.Run with an already-cancelled ctx still
		// completes deletePending and removes the sidecar before publishing
		// FinalDir (cleanup past the point of no return is non-abandonable),
		// and the sidecar remains present during the unlink loop until the loop
		// finishes.
		cancelJob, cancelDir := stageJob(t)
		cancelFinal := filepath.Join(baseDir, "complete", "Cancelled.Finalize")
		cancelJob.FinalDir = cancelFinal
		cancelJob.PendingDeletions = []string{"part1.rar", "part2.par2"}
		for name, content := range map[string]string{
			"movie.mkv":  "delivered",
			"part1.rar":  "archive-1",
			"part2.par2": "par2-index",
		} {
			if err := os.WriteFile(filepath.Join(cancelDir, name), []byte(content), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		var sidecarPresentDuringLoop bool
		finalize.Log = slog.New(&hookHandler{
			Handler: logger.Handler(),
			onRecord: func(msg string) {
				if msg == "Deleted part1.rar" {
					if _, err := os.Stat(filepath.Join(cancelJob.DownloadDir, pendingDeletionsFile)); err == nil {
						sidecarPresentDuringLoop = true
					}
				}
			},
		})
		cancelledCtx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := finalize.Run(cancelledCtx, cancelJob); err != nil {
			t.Fatalf("finalize.Run(cancelledCtx): %v", err)
		}
		finalize.Log = logger
		if !sidecarPresentDuringLoop {
			t.Error("expected .gonzbd-pending-deletions sidecar to exist during deletePending loop and be removed only after the loop")
		}
		assertPresent(t, cancelFinal, "movie.mkv")
		assertAbsent(t, cancelFinal, "part1.rar", "part2.par2", pendingDeletionsFile)

		// 5. Simulate a SIGKILL partway through deletePending in _UNPACK_<FinalDir>
		// by seeding _UNPACK_<FinalDir> with the JSON sidecar (listing part1.rar
		// and part3.par2, with part1.rar already unlinked before SIGKILL and
		// part3.par2 still present). On restart, recoverUnpackFinalDir reads the
		// sidecar, unlinks part3.par2, removes the sidecar, and renames
		// _UNPACK_<FinalDir> to FinalDir.
		missingDownloadDir := filepath.Join(baseDir, "incomplete", "Crashed.Finalize")
		subFinalDir := filepath.Join(baseDir, "complete", "Crashed.Finalize")
		unpackFinalDir := prefixDirName(subFinalDir, "_UNPACK_")
		if err := os.MkdirAll(unpackFinalDir, 0o750); err != nil {
			t.Fatalf("mkdir unpackFinalDir: %v", err)
		}
		for name, content := range map[string]string{
			"movie.mkv":          "delivered",
			"part3.par2":         "par2-index",
			pendingDeletionsFile: `["part1.rar","part3.par2"]`,
		} {
			if err := os.WriteFile(filepath.Join(unpackFinalDir, name), []byte(content), 0o600); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}

		logBuf.Reset()
		job2 := makeJob(t, "crashed-finalize")
		job2.DownloadDir = missingDownloadDir
		job2.FinalDir = subFinalDir
		job2.PP = types.PPDelete

		pp := New(Options{
			Stages: []Stage{finalize},
			Logger: logger,
		})
		pp.processJob(t.Context(), job2)

		if job2.FailMsg != "" {
			t.Fatalf("FailMsg = %q, want empty", job2.FailMsg)
		}
		if job2.DownloadDir != subFinalDir {
			t.Errorf("job2.DownloadDir = %q, want %q", job2.DownloadDir, subFinalDir)
		}
		if !strings.Contains(logBuf.String(), "postproc: recovered staged _UNPACK_ directory to FinalDir") {
			t.Errorf("expected recovery Info log, got %q", logBuf.String())
		}
		assertPresent(t, subFinalDir, "movie.mkv")
		assertAbsent(t, subFinalDir, "part1.rar", "part3.par2", pendingDeletionsFile)
		if _, err := os.Stat(unpackFinalDir); !os.IsNotExist(err) {
			t.Errorf("expected %s to be renamed to %s, stat err=%v", unpackFinalDir, subFinalDir, err)
		}

		roComplete := filepath.Join(baseDir, "ro-complete")
		roFinal := filepath.Join(roComplete, "Crashed.RO")
		roUnpack := prefixDirName(roFinal, "_UNPACK_")
		if err := os.MkdirAll(roUnpack, 0o750); err != nil {
			t.Fatalf("mkdir roUnpack: %v", err)
		}
		if err := os.WriteFile(filepath.Join(roUnpack, "movie.mkv"), []byte("delivered"), 0o600); err != nil {
			t.Fatalf("write movie.mkv: %v", err)
		}
		if err := os.Chmod(roComplete, 0o500); err != nil {
			t.Fatalf("chmod roComplete: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(roComplete, 0o700) })

		logBuf.Reset()
		roJob := makeJob(t, "crashed-ro")
		roJob.DownloadDir = missingDownloadDir
		roJob.FinalDir = roFinal
		pp.processJob(t.Context(), roJob)

		if !strings.Contains(logBuf.String(), "postproc: failed to recover staged _UNPACK_ directory") {
			t.Errorf("expected recovery Warn log, got %q", logBuf.String())
		}
		if !strings.Contains(roJob.FailMsg, roUnpack) {
			t.Errorf("roJob.FailMsg = %q, want mention of %s", roJob.FailMsg, roUnpack)
		}

		if err := pp.recoverUnpackFinalDir(t.Context(), &Job{FlatLayout: true, FinalDir: subFinalDir}); err != nil {
			t.Errorf("recoverUnpackFinalDir(FlatLayout=true) = %v, want nil", err)
		}
	})
}

// TestDeferredCleanup_CleanupFlagsHonouredAtFinalize verifies #768:
//   - A successful run removes extracted archives from FinalDir iff unpack
//     cleanup is enabled, and removes par2 + par2 backup files from FinalDir
//     iff par2 cleanup is enabled.
//   - A failed finalize (e.g. read-only destination parent) deletes nothing.
func TestDeferredCleanup_CleanupFlagsHonouredAtFinalize(t *testing.T) {
	t.Parallel()

	intactRar := intactLayoutARar(t)
	for _, tc := range []struct {
		name          string
		unpackCleanup bool
		par2Cleanup   bool
	}{
		{name: "both cleanup flags on", unpackCleanup: true, par2Cleanup: true},
		{name: "unpack cleanup on, par2 cleanup off", unpackCleanup: true, par2Cleanup: false},
		{name: "unpack cleanup off, par2 cleanup on", unpackCleanup: false, par2Cleanup: true},
		{name: "both cleanup flags off", unpackCleanup: false, par2Cleanup: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			job, dir := layoutJob(t, "layout_a", []string{"Real.Name.par2", "Real.Name.vol0+2.par2"},
				intactRar, "Real.Name.rar")
			job.Job.SetName("Real.Name")
			// Add a par2 repair backup file alongside Real.Name.rar and an .sfv file
			// for extension_cleanup.
			if err := os.WriteFile(filepath.Join(dir, "Real.Name.rar.1"), []byte("damaged-backup"), 0o600); err != nil {
				t.Fatalf("write backup: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "checksums.sfv"), []byte("sfv"), 0o600); err != nil {
				t.Fatalf("write sfv: %v", err)
			}

			finalDir := filepath.Join(t.TempDir(), "complete", "Real.Name")
			job.FinalDir = finalDir

			// Run only when cleanup is enabled for unwanted/ext check so kept
			// archives/par2 files aren't rejected by the test's unwanted rule when
			// cleanup is off.
			discard := slog.New(slog.DiscardHandler)
			up := NewUnpackStageWith(unpack.Options{UseGoRAR: true, UseGo7z: true}, tc.unpackCleanup)
			up.SetEnabled(true)
			up.Log = discard
			qc := &QuickCheckStage{Log: discard, Unpack: up}
			qc.SetEnabled(true)
			repair := &RepairStage{UseGoPar2: true, Log: discard}
			par2Clean := NewPar2CleanupStage(tc.par2Cleanup)
			par2Clean.Log = discard
			extClean := NewExtensionCleanupStage([]string{"sfv"})
			extClean.Log = discard
			finalize := NewFinalizeStage()
			finalize.Log = discard

			pp := New(Options{
				Stages: []Stage{qc, repair, up, par2Clean, extClean, finalize},
				Logger: discard,
			})
			pp.processJob(t.Context(), job)

			if job.ParError || job.UnpackError || job.FailMsg != "" {
				t.Fatalf("ParError=%v UnpackError=%v FailMsg=%q", job.ParError, job.UnpackError, job.FailMsg)
			}

			if tc.unpackCleanup || tc.par2Cleanup {
				cleanedIdx, strippedIdx := -1, -1
				for _, entry := range job.StageLog {
					if entry.Stage != "finalize" {
						continue
					}
					for i, line := range entry.Lines {
						if strings.HasPrefix(line, "Cleaned up ") {
							cleanedIdx = i
						}
						if strings.HasSuffix(line, "(prefix stripped)") {
							strippedIdx = i
						}
					}
				}
				if cleanedIdx < 0 || strippedIdx < 0 || cleanedIdx >= strippedIdx {
					t.Errorf("finalize StageLog = %+v, want 'Cleaned up ...' (idx %d) before '(prefix stripped)' (idx %d)",
						job.StageLog, cleanedIdx, strippedIdx)
				}
			}

			assertFeatureExtracted(t, finalDir)
			assertAbsent(t, finalDir, "checksums.sfv")

			if tc.unpackCleanup {
				assertAbsent(t, finalDir, "Real.Name.rar")
			} else {
				assertPresent(t, finalDir, "Real.Name.rar")
			}

			if tc.par2Cleanup {
				assertAbsent(t, finalDir, "Real.Name.par2", "Real.Name.vol0+2.par2", "Real.Name.rar.1")
			} else {
				assertPresent(t, finalDir, "Real.Name.par2", "Real.Name.vol0+2.par2", "Real.Name.rar.1")
			}
		})
	}

	t.Run("failed finalize deletes nothing", func(t *testing.T) {
		t.Parallel()
		if os.Getuid() == 0 {
			t.Skip("root bypasses read-only directory permissions")
		}

		job, dir := layoutJob(t, "layout_a", []string{"Real.Name.par2", "Real.Name.vol0+2.par2"},
			intactRar, "Real.Name.rar")
		job.Job.SetName("Real.Name")
		if err := os.WriteFile(filepath.Join(dir, "Real.Name.rar.1"), []byte("damaged-backup"), 0o600); err != nil {
			t.Fatalf("write backup: %v", err)
		}

		roParent := filepath.Join(t.TempDir(), "readonly")
		if err := os.MkdirAll(roParent, 0o500); err != nil {
			t.Fatalf("mkdir readonly: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(roParent, 0o700) })
		job.FinalDir = filepath.Join(roParent, "sub", "Real.Name")

		pp := New(Options{
			Stages: buildPipelineForDeferredCleanup(true, true),
			Logger: slog.New(slog.DiscardHandler),
		})
		pp.processJob(t.Context(), job)

		if job.FailMsg == "" {
			t.Fatal("expected FinalizeStage move failure to set job.FailMsg")
		}
		assertPresent(t, dir, "Real.Name.rar", "Real.Name.par2", "Real.Name.vol0+2.par2", "Real.Name.rar.1", "feature.bin")
	})

	t.Run("already at final location deletes pending files", func(t *testing.T) {
		t.Parallel()

		job, dir := layoutJob(t, "layout_a", []string{"Real.Name.par2", "Real.Name.vol0+2.par2"},
			intactRar, "Real.Name.rar")
		job.Job.SetName("Real.Name")
		job.FinalDir = dir

		pp := New(Options{
			Stages: buildPipelineForDeferredCleanup(true, true),
			Logger: slog.New(slog.DiscardHandler),
		})
		pp.processJob(t.Context(), job)

		if job.ParError || job.UnpackError || job.FailMsg != "" {
			t.Fatalf("ParError=%v UnpackError=%v FailMsg=%q", job.ParError, job.UnpackError, job.FailMsg)
		}
		assertFeatureExtracted(t, dir)
		assertAbsent(t, dir, "Real.Name.rar", "Real.Name.par2", "Real.Name.vol0+2.par2")
	})

	t.Run("moveFileByFile skips copying pending files to FinalDir and deletes them in place", func(t *testing.T) {
		t.Parallel()

		job, dir := stageJob(t)
		job.Job.SetName("Real.Name")
		job.Sanitize = fsutil.SanitizeOptions{ReplaceSpacesWith: "_"}
		finalDir := filepath.Join(t.TempDir(), "complete", "Real.Name")
		job.FinalDir = finalDir

		// Pre-populate finalDir so FinalizeStage's atomic rename falls back to
		// moveFileByFile.
		if err := os.MkdirAll(finalDir, 0o750); err != nil {
			t.Fatalf("mkdir finalDir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(finalDir, "existing.txt"), []byte("keep"), 0o600); err != nil {
			t.Fatalf("write existing.txt: %v", err)
		}

		rarBytes, err := os.ReadFile(unpackFixture("single_rar5.rar"))
		if err != nil {
			t.Fatalf("read rar fixture: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "My Release.rar"), rarBytes, 0o600); err != nil {
			t.Fatalf("write My Release.rar: %v", err)
		}
		subsDir := filepath.Join(dir, "Subs")
		if err := os.MkdirAll(subsDir, 0o750); err != nil {
			t.Fatalf("mkdir Subs: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subsDir, "en.srt"), []byte("subtitle"), 0o600); err != nil {
			t.Fatalf("write Subs/en.srt: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subsDir, "inner.rar"), rarBytes, 0o600); err != nil {
			t.Fatalf("write Subs/inner.rar: %v", err)
		}

		discard := slog.New(slog.DiscardHandler)
		up := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true)
		up.SetEnabled(true)
		up.EnableRecursive = true
		up.Log = discard
		finalize := NewFinalizeStage()
		finalize.Log = discard

		for _, stage := range []Stage{up, finalize} {
			if err := stage.Run(t.Context(), job); err != nil {
				t.Fatalf("%s.Run: %v", stage.Name(), err)
			}
		}

		for _, line := range job.OutputLines {
			if strings.Contains(line, "My Release.rar →") {
				t.Errorf("moveFileByFile copied pending archive into FinalDir: %q", line)
			}
		}
		assertPresent(t, finalDir, "existing.txt", filepath.Join("Subs", "en.srt"))
		assertAbsent(t, finalDir, "My Release.rar", "My_Release.rar", filepath.Join("Subs", "inner.rar"), pendingDeletionsFile)
	})
}

// writeMinimalPar2File writes a valid PAR2 FileDesc packet in path mapping
// data's 16 KB MD5 hash to targetName.
func writeMinimalPar2File(t *testing.T, path, targetName string, data []byte) {
	t.Helper()
	sum16k := md5.Sum(data[:min(16384, len(data))]) //nolint:gosec // PAR2 spec uses MD5
	nameBytes := []byte(targetName)
	if pad := (4 - len(nameBytes)%4) % 4; pad > 0 {
		nameBytes = append(nameBytes, make([]byte, pad)...)
	}
	bodyLen := uint64(16 + 16 + 16 + 8 + len(nameBytes))
	pktLen := 64 + bodyLen
	buf := make([]byte, pktLen)
	copy(buf[0:8], "PAR2\x00PKT")
	binary.LittleEndian.PutUint64(buf[8:16], pktLen)
	copy(buf[48:64], "PAR 2.0\x00FileDesc")
	copy(buf[64+32:64+48], sum16k[:])
	binary.LittleEndian.PutUint64(buf[64+48:64+56], uint64(len(data)))
	copy(buf[64+56:], nameBytes)
	pktSum := md5.Sum(buf[32:]) //nolint:gosec // PAR2 spec uses MD5
	copy(buf[16:32], pktSum[:])
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write par2 %s: %v", path, err)
	}
}

// writeSingleFileTar writes a valid .tar archive at path containing one
// regular file entry (memberName -> memberData).
func writeSingleFileTar(t *testing.T, path, memberName string, memberData []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: memberName,
		Mode: 0o600,
		Size: int64(len(memberData)),
	}); err != nil {
		t.Fatalf("tar WriteHeader: %v", err)
	}
	if _, err := tw.Write(memberData); err != nil {
		t.Fatalf("tar Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	tarBytes := buf.Bytes()
	if err := os.WriteFile(path, tarBytes, 0o600); err != nil {
		t.Fatalf("write tar %s: %v", path, err)
	}
	return tarBytes
}

// TestDeferredCleanup_RerunAfterDeobfuscateOrPar2Rename verifies that when
// Run 1 extracts an obfuscated payload file from an archive and deobfuscate or
// recover_par2_names renames that extracted file before a crash prior to
// FinalizeStage, Run 2 deduplicates the re-extracted obfuscated file against
// the already-renamed target by content and delivers only the single renamed
// payload file to FinalDir (#768).
func TestDeferredCleanup_RerunAfterDeobfuscateOrPar2Rename(t *testing.T) {
	t.Parallel()

	t.Run("rerun after deobfuscate renamed extracted payload", func(t *testing.T) {
		t.Parallel()

		job1, dir := stageJob(t)
		job1.PP = types.PPDelete
		job1.Job.SetName("Clean.Movie.2026")
		finalDir := filepath.Join(t.TempDir(), "complete", "Clean.Movie.2026")
		job1.FinalDir = finalDir

		payload := make([]byte, 11*1024*1024)
		copy(payload, "video-payload")
		writeSingleFileTar(t, filepath.Join(dir, "archive.tar"), "b082fa0beaa644d3aa01045d5b8d0b36.mkv", payload)

		discard := slog.New(slog.DiscardHandler)
		makeStages := func(extra ...Stage) []Stage {
			up := NewUnpackStageWith(unpack.Options{}, true)
			up.SetEnabled(true)
			up.EnableTar = true
			up.Log = discard
			deob := NewDeobfuscateStage()
			deob.SetEnabled(true)
			deob.Log = discard
			finalize := NewFinalizeStage()
			finalize.Log = discard
			s := make([]Stage, 0, 3+len(extra))
			s = append(s, up, deob)
			s = append(s, extra...)
			s = append(s, finalize)
			return s
		}

		ctx1, cancel1 := context.WithCancel(t.Context())
		defer cancel1()
		pp1 := New(Options{
			Stages: makeStages(abortBeforeFinalizeStage{cancel: cancel1}),
			Logger: discard,
		})
		pp1.processJob(ctx1, job1)

		assertPresent(t, dir, "archive.tar", "Clean.Movie.2026.mkv")
		assertAbsent(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv")

		diffPayload := make([]byte, len(payload))
		copy(diffPayload, "different-video-payload")
		if err := os.WriteFile(filepath.Join(dir, "0675e29e9abfd2f7d069dab0b853283c.mkv"), diffPayload, 0o600); err != nil {
			t.Fatalf("write diff obfuscated mkv: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Keep.Notes.mkv"), payload, 0o600); err != nil {
			t.Fatalf("write Keep.Notes.mkv: %v", err)
		}

		job2 := &Job{
			Job:         job1.Job,
			DownloadDir: dir,
			FinalDir:    finalDir,
			PP:          types.PPDelete,
		}
		pp2 := New(Options{
			Stages: makeStages(),
			Logger: discard,
		})
		pp2.processJob(t.Context(), job2)

		assertPresent(t, finalDir, "Clean.Movie.2026.mkv", "0675e29e9abfd2f7d069dab0b853283c.mkv", "Keep.Notes.mkv")
		assertAbsent(t, finalDir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", "archive.tar")
	})

	t.Run("rerun after recover_par2_names renamed extracted payload", func(t *testing.T) {
		t.Parallel()

		job1, dir := stageJob(t)
		job1.PP = types.PPDelete
		job1.Job.SetName("Fallback.Job.Name")
		finalDir := filepath.Join(t.TempDir(), "complete", "Par2.Movie.2026")
		job1.FinalDir = finalDir

		payload := make([]byte, 32*1024)
		copy(payload, "par2-named-video-payload")
		writeSingleFileTar(t, filepath.Join(dir, "archive.tar"), "b082fa0beaa644d3aa01045d5b8d0b36.mkv", payload)
		writeMinimalPar2File(t, filepath.Join(dir, "movie.par2"), "Par2.Movie.2026.mkv", payload)

		discard := slog.New(slog.DiscardHandler)
		makeStages := func(extra ...Stage) []Stage {
			up := NewUnpackStageWith(unpack.Options{}, true)
			up.SetEnabled(true)
			up.EnableTar = true
			up.Log = discard
			par2Names := NewRecoverPar2NamesStage()
			par2Names.Log = discard
			par2Clean := NewPar2CleanupStage(true)
			par2Clean.Log = discard
			finalize := NewFinalizeStage()
			finalize.Log = discard
			s := make([]Stage, 0, 4+len(extra))
			s = append(s, up, par2Names, par2Clean)
			s = append(s, extra...)
			s = append(s, finalize)
			return s
		}

		ctx1, cancel1 := context.WithCancel(t.Context())
		defer cancel1()
		pp1 := New(Options{
			Stages: makeStages(abortBeforeFinalizeStage{cancel: cancel1}),
			Logger: discard,
		})
		pp1.processJob(ctx1, job1)

		assertPresent(t, dir, "archive.tar", "movie.par2", "Par2.Movie.2026.mkv")
		assertAbsent(t, dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv")

		job2 := &Job{
			Job:         job1.Job,
			DownloadDir: dir,
			FinalDir:    finalDir,
			PP:          types.PPDelete,
		}
		pp2 := New(Options{
			Stages: makeStages(),
			Logger: discard,
		})
		pp2.processJob(t.Context(), job2)

		assertPresent(t, finalDir, "Par2.Movie.2026.mkv")
		assertAbsent(t, finalDir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv", "archive.tar", "movie.par2")
	})

	t.Run("first run extracts both main.rar yielding JobName.txt and extras.tar yielding notes.txt", func(t *testing.T) {
		t.Parallel()

		job, dir := stageJob(t)
		job.PP = types.PPDelete
		// sample.rar extracts "sample.txt"; setting Job.Name to "sample" means
		// main.rar yields <JobName>.txt ("sample.txt") while extras.tar yields
		// "notes.txt" with the same .txt extension.
		job.Job.SetName("sample")
		finalDir := filepath.Join(t.TempDir(), "complete", "sample")
		job.FinalDir = finalDir

		rarBytes, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "rar", "sample.rar"))
		if err != nil {
			t.Fatalf("read sample.rar: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.rar"), rarBytes, 0o600); err != nil {
			t.Fatalf("write main.rar: %v", err)
		}
		writeSingleFileTar(t, filepath.Join(dir, "extras.tar"), "notes.txt", []byte("extra release notes"))

		discard := slog.New(slog.DiscardHandler)
		up := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true)
		up.SetEnabled(true)
		up.EnableTar = true
		up.Log = discard
		deob := NewDeobfuscateStage()
		deob.SetEnabled(true)
		deob.Log = discard
		finalize := NewFinalizeStage()
		finalize.Log = discard

		pp := New(Options{
			Stages: []Stage{up, deob, finalize},
			Logger: discard,
		})
		pp.processJob(t.Context(), job)

		assertPresent(t, finalDir, "sample.txt", "notes.txt")
		assertAbsent(t, finalDir, "main.rar", "extras.tar")
	})

	t.Run("first run extracts main.rar alongside par2-protected nfo", func(t *testing.T) {
		t.Parallel()

		job, dir := stageJob(t)
		job.PP = types.PPDelete
		job.Job.SetName("My.Release")
		finalDir := filepath.Join(t.TempDir(), "complete", "My.Release")
		job.FinalDir = finalDir

		rarBytes, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "rar", "sample.rar"))
		if err != nil {
			t.Fatalf("read sample.rar: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.rar"), rarBytes, 0o600); err != nil {
			t.Fatalf("write main.rar: %v", err)
		}
		nfoData := []byte("release nfo protected by par2")
		if err := os.WriteFile(filepath.Join(dir, "000-obfuscated.nfo"), nfoData, 0o600); err != nil {
			t.Fatalf("write 000-obfuscated.nfo: %v", err)
		}
		writeMinimalPar2File(t, filepath.Join(dir, "release.par2"), "Release.nfo", nfoData)

		discard := slog.New(slog.DiscardHandler)
		par2Names := NewRecoverPar2NamesStage()
		par2Names.Log = discard
		up := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true)
		up.SetEnabled(true)
		up.Log = discard
		par2Clean := NewPar2CleanupStage(true)
		par2Clean.Log = discard
		finalize := NewFinalizeStage()
		finalize.Log = discard

		pp := New(Options{
			Stages: []Stage{par2Names, up, par2Clean, finalize},
			Logger: discard,
		})
		pp.processJob(t.Context(), job)

		assertPresent(t, finalDir, "Release.nfo", "sample.txt")
		assertAbsent(t, finalDir, "000-obfuscated.nfo", "main.rar", "release.par2")
	})
}

// TestDeferredCleanup_RepairSetsExcludesPendingDeletionsFromDataFiles verifies
// that repairSets excludes job.PendingDeletions from the extra dataFiles passed
// to par2 (#768).
func TestDeferredCleanup_RepairSetsExcludesPendingDeletionsFromDataFiles(t *testing.T) {
	t.Parallel()

	intactRar := intactLayoutARar(t)
	job, dir := layoutJob(t, "layout_a", []string{"Real.Name.par2"}, intactRar, "Real.Name.rar")
	if err := os.WriteFile(filepath.Join(dir, "pending_archive.rar"), []byte("pending"), 0o600); err != nil {
		t.Fatalf("write pending_archive.rar: %v", err)
	}
	job.recordPendingDeletion("pending_archive.rar")

	repair := &RepairStage{UseGoPar2: true, Log: slog.New(slog.DiscardHandler)}
	if err := repair.Run(t.Context(), job); err != nil {
		t.Fatalf("RepairStage.Run: %v", err)
	}
	if !slices.Contains(job.OutputLines, "Found 1 non-par2 data file(s) for checksum matching") {
		t.Errorf("expected RepairStage to exclude pending_archive.rar from dataFiles (want 1 data file), got OutputLines: %v", job.OutputLines)
	}
}

// TestDeferredCleanup_ExcludedFromIntermediateStages verifies #768:
// files recorded for deletion at finalize (extracted archives, par2 files, and
// par2 repair backups) are ignored by sample_cleanup, recover_par2_names,
// deobfuscate (neither blocking the 3x biggest-file ratio check nor getting
// magic-byte extension fixes on .1 backups), unwanted_cleanup, and
// extension_cleanup before finalize runs.
func TestDeferredCleanup_ExcludedFromIntermediateStages(t *testing.T) {
	t.Parallel()

	job, dir := stageJob(t)
	job.Job.SetName("Clean.Movie.2026")
	finalDir := filepath.Join(t.TempDir(), "complete", "Clean.Movie.2026")
	job.FinalDir = finalDir

	// Create an 11 MiB obfuscated video and a matching .srt subtitle.
	mkvPath := filepath.Join(dir, "b082fa0beaa644d3aa01045d5b8d0b36.mkv")
	if err := os.WriteFile(mkvPath, make([]byte, 11*1024*1024), 0o600); err != nil {
		t.Fatalf("write mkv: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "english.srt"), []byte("subtitle"), 0o600); err != nil {
		t.Fatalf("write srt: %v", err)
	}

	// Copy a real RAR5 archive into dir under a "sample_*" name and pad a par2
	// backup file with RAR5 magic bytes to 10 MiB. Without excluding pending
	// deletions:
	//   - sample_cleanup deletes sample_archive.rar before finalize;
	//   - recover_par2_names renames sample_archive.tar to Par2_Renamed.tar;
	//   - deobfuscate's 10 MiB backup file defeats BiggestFile's 3x ratio guard
	//     against the 11 MiB video AND FixExtension renames the .1 backup to .1.rar;
	//   - unwanted_cleanup and extension_cleanup delete the pending files before finalize.
	rarBytes, err := os.ReadFile(unpackFixture("single_rar5.rar"))
	if err != nil {
		t.Fatalf("read rar fixture: %v", err)
	}
	for _, name := range []string{"sample_archive.rar", "movie_part.rar"} {
		if err := os.WriteFile(filepath.Join(dir, name), rarBytes, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	backupBytes := make([]byte, 10*1024*1024)
	copy(backupBytes, rarBytes)
	if err := os.WriteFile(filepath.Join(dir, "movie_part.rar.1"), backupBytes, 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	tarBytes := writeSingleFileTar(t, filepath.Join(dir, "sample_archive.tar"), "extra.txt", []byte("extra payload"))
	writeMinimalPar2File(t, filepath.Join(dir, "movie.par2"), "Par2_Renamed.tar", tarBytes)

	discard := slog.New(slog.DiscardHandler)
	up := NewUnpackStageWith(unpack.Options{UseGoRAR: true}, true)
	up.SetEnabled(true)
	up.EnableTar = true
	up.Log = discard

	sampleClean := NewSampleCleanupStage()
	sampleClean.SetEnabled(true)
	sampleClean.Log = discard

	par2Names := NewRecoverPar2NamesStage()
	par2Names.Log = discard

	par2Clean := NewPar2CleanupStage(true)
	par2Clean.Log = discard

	deob := NewDeobfuscateStage()
	deob.SetEnabled(true)
	deob.Log = discard

	// Configure unwanted_cleanup to delete .rar, .tar, .par2, or .1 if it
	// doesn't skip pending deletions.
	unwantedStage := NewUnwantedCleanupStage(func() (unwanted.Rules, error) {
		return unwanted.NewRules(unwanted.ActionFail, unwanted.ModeBlacklist, []string{"rar", "tar", "par2", "1"})
	})
	unwantedStage.Log = discard

	// Configure extension_cleanup to delete .rar, .tar, .par2, .1 if it doesn't
	// skip pending deletions.
	extClean := NewExtensionCleanupStage([]string{"rar", "tar", "par2", "1"})
	extClean.Log = discard

	for _, stage := range []Stage{up, sampleClean, par2Names, par2Clean, deob, unwantedStage, extClean} {
		if err := stage.Run(t.Context(), job); err != nil {
			t.Fatalf("%s.Run: %v", stage.Name(), err)
		}
	}

	if job.FailMsg != "" {
		t.Fatalf("intermediate stage set FailMsg = %q", job.FailMsg)
	}

	// Before FinalizeStage runs, the archives, backup, and par2 file must still
	// exist in DownloadDir (not deleted by sample_cleanup, unwanted_cleanup, or
	// extension_cleanup, and not renamed by recover_par2_names or deobfuscate),
	// while the 11 MiB video and subtitle WERE deobfuscated!
	assertPresent(t, dir,
		"sample_archive.rar",
		"movie_part.rar",
		"sample_archive.tar",
		"movie_part.rar.1",
		"movie.par2",
		"Clean.Movie.2026.mkv",
		"Clean.Movie.2026.english.srt",
		"extra.txt",
	)
	assertAbsent(t, dir, "movie_part.rar.1.rar", "Par2_Renamed.tar")

	// Now run FinalizeStage: it moves everything to FinalDir and deletes the
	// pending archive, par2, and backup files.
	finalize := NewFinalizeStage()
	finalize.Log = discard
	if err := finalize.Run(t.Context(), job); err != nil {
		t.Fatalf("finalize.Run: %v", err)
	}

	assertPresent(t, finalDir, "Clean.Movie.2026.mkv", "Clean.Movie.2026.english.srt", "extra.txt")
	assertAbsent(t, finalDir, "sample_archive.rar", "movie_part.rar", "sample_archive.tar", "movie_part.rar.1", "movie.par2")
}

func TestPendingDeletionHelpers(t *testing.T) {
	t.Parallel()

	parentDir := t.TempDir()
	dir := filepath.Join(parentDir, "download")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir download: %v", err)
	}
	j := &Job{
		DownloadDir: dir,
		Sanitize:    fsutil.SanitizeOptions{ReplaceSpacesWith: "_"},
	}

	if set := j.pendingDeletionSet(); set != nil {
		t.Errorf("empty pendingDeletionSet() = %v, want nil", set)
	}
	if j.isPendingDeletion("movie.rar") {
		t.Error("empty isPendingDeletion(movie.rar) = true, want false")
	}

	// Reject empty, dot, parent traversal, and paths when DownloadDir is unset.
	for _, bad := range []string{"", ".", "..", "../escape.rar", filepath.Join(parentDir, "outside.rar")} {
		if rel, ok := j.pendingDeletionRel(bad); ok {
			t.Errorf("pendingDeletionRel(%q) = (%q, true), want false", bad, rel)
		}
		if j.recordPendingDeletion(bad) {
			t.Errorf("recordPendingDeletion(%q) = true, want false", bad)
		}
	}
	noDirJob := &Job{}
	if _, ok := noDirJob.pendingDeletionRel(filepath.Join(dir, "movie.rar")); ok {
		t.Error("pendingDeletionRel with empty DownloadDir and absolute path = true, want false")
	}

	// Record relative and absolute paths, deduplicating entries.
	if !j.recordPendingDeletion(filepath.Join(dir, "movie.rar")) {
		t.Fatal("recordPendingDeletion(abs movie.rar) = false, want true")
	}
	if !j.recordPendingDeletion("movie.rar") {
		t.Fatal("recordPendingDeletion(rel movie.rar) = false, want true")
	}
	if !j.recordPendingDeletion("My Dir/part.par2") {
		t.Fatal("recordPendingDeletion(My Dir/part.par2) = false, want true")
	}
	if len(j.PendingDeletions) != 2 {
		t.Fatalf("PendingDeletions = %v, want 2 deduplicated entries", j.PendingDeletions)
	}
	if !j.isPendingDeletion(filepath.Join(dir, "movie.rar")) || !j.isPendingDeletion("My Dir/part.par2") {
		t.Errorf("isPendingDeletion failed for recorded entries: %v", j.PendingDeletions)
	}
	set := j.pendingDeletionSet()
	if _, ok := set["movie.rar"]; !ok {
		t.Errorf("pendingDeletionSet missing movie.rar: %v", set)
	}

	// Verify deletePending:
	//   1. Removes movie.rar inside DownloadDir;
	//   2. Unlinks "My Dir/part.par2" when the top-level directory was
	//      sanitized to "My_Dir/part.par2" (preserving nested separators);
	//   3. Refuses "../outside.txt" and leaves parentDir/outside.txt intact.
	if err := os.WriteFile(filepath.Join(dir, "movie.rar"), []byte("rar"), 0o600); err != nil {
		t.Fatalf("write movie.rar: %v", err)
	}
	sanitizedSubDir := filepath.Join(dir, "My_Dir")
	if err := os.MkdirAll(sanitizedSubDir, 0o750); err != nil {
		t.Fatalf("mkdir My_Dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sanitizedSubDir, "part.par2"), []byte("par2"), 0o600); err != nil {
		t.Fatalf("write My_Dir/part.par2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parentDir, "outside.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write outside.txt: %v", err)
	}
	j.PendingDeletions = append(j.PendingDeletions, "../outside.txt")

	f := NewFinalizeStage()
	f.Log = slog.New(slog.DiscardHandler)
	f.deletePending(t.Context(), f.Log, j)
	assertAbsent(t, dir, "movie.rar")
	assertAbsent(t, sanitizedSubDir, "part.par2")
	assertPresent(t, parentDir, "outside.txt")

	var u UnwantedCleanupStage
	if err := u.fail(j, errors.New("bad.exe")); err == nil || j.FailMsg == "" {
		t.Errorf("u.fail() = %v, FailMsg = %q, want non-nil error and FailMsg", err, j.FailMsg)
	}
}
