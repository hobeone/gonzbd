// export_test.go exposes internal state for white-box testing.
// This file is compiled only during test builds.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/checkpoint"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// ForceAssemblerStopped starts then stops the assembler so it is in a
// permanently-stopped state. A subsequent app.Start will fail at the
// assembler.Start step (assembler returns ErrStopped). Used to test that a
// failed Start resets started=false so the application can be retried.
func (a *Application) ForceAssemblerStopped() error {
	if err := a.assembler.Start(a.ctx); err != nil {
		return err
	}
	return a.assembler.Stop()
}

// GetConfig returns the application config for testing.
func (a *Application) GetConfig() *config.Config {
	return a.config
}

// Checkpointer returns the application's checkpointer for testing.
func (a *Application) Checkpointer() *checkpoint.Checkpointer {
	return a.checkpointer
}

// TriggerMaybeDirectUnpack drives the DirectUnpack orchestrator's start path.
func (a *Application) TriggerMaybeDirectUnpack(fc FileComplete) {
	a.duOrch.maybeStart(fc)
}

// TriggerBuildDirectUnpackOpts calls the orchestrator's option builder.
func (a *Application) TriggerBuildDirectUnpackOpts() any {
	return a.duOrch.buildOpts(false, false, false)
}

// TriggerPersistAndCommit calls the finalizer's persistAndCommit method.
func (a *Application) TriggerPersistAndCommit(log *slog.Logger, entry history.Entry, job *postproc.Job) error {
	return a.finalizer.persistAndCommit(log, entry, job)
}

// InjectDirectUnpacker injects a direct unpacker for testing.
func (a *Application) InjectDirectUnpacker(jobID string, du *directunpack.DirectUnpacker) {
	a.duOrch.inject(jobID, du)
}

// TriggerBuildDownloaderOptions calls the unexported buildDownloaderOptions method.
func (a *Application) TriggerBuildDownloaderOptions() downloader.Options {
	return a.buildDownloaderOptions()
}

// TriggerHandleFileComplete calls the unexported handleFileComplete method.
func (a *Application) TriggerHandleFileComplete(ctx context.Context, fc FileComplete) {
	a.handleFileComplete(ctx, fc)
}

// TriggerDrainCompletions calls the unexported drainCompletions method.
func (a *Application) TriggerDrainCompletions(ctx context.Context) {
	a.drainCompletions(ctx)
}

// SendFileComplete sends a FileComplete event to the internal channel.
func (a *Application) SendFileComplete(fc FileComplete) {
	a.internalFileComplete <- fc
}

// TriggerMaybeFinalize calls the unexported maybeFinalize method.
func (a *Application) TriggerMaybeFinalize(jobID, failMsg string) {
	a.maybeFinalize(jobID, failMsg)
}

// InjectPipelineFileInfo inserts a file path in the pipeline's fileInfo cache.
func (a *Application) InjectPipelineFileInfo(jobID string, fileIdx int, path string) {
	a.pipeline.mu.Lock()
	defer a.pipeline.mu.Unlock()
	a.pipeline.fileInfo[fileKey{jobID: jobID, fileIdx: fileIdx}] = assembler.FileInfo{Path: path}
}

// InjectCtx injects a lifecycle context.
func (a *Application) InjectCtx(ctx context.Context) {
	a.ctx = ctx
}

// InjectCancel injects a lifecycle cancel function.
func (a *Application) InjectCancel(cancel context.CancelFunc) {
	a.cancel = cancel
}

// SetActiveDU sets the DirectUnpack active count.
func (a *Application) SetActiveDU(val int32) {
	a.duOrch.setActive(int(val))
}

// SetStarted sets the started state for testing.
func (a *Application) SetStarted(val bool) {
	a.started.Store(val)
}

// TriggerFireCompletionNotification calls the finalizer's fireCompletionNotification method.
func (a *Application) TriggerFireCompletionNotification(entry history.Entry) {
	a.finalizer.fireCompletionNotification(entry)
}

// FinalizerIsInFlight reports whether the given job ID is tracked as in-flight by the finalizer.
func (a *Application) FinalizerIsInFlight(id string) bool {
	if a.finalizer == nil {
		return false
	}
	return a.finalizer.isInFlight(id)
}

// TriggerOnFileComplete invokes the OnFileComplete callback directly for testing.
func (a *Application) TriggerOnFileComplete(jobID string, fileIdx int) {
	if a.onFileComplete != nil {
		a.onFileComplete(jobID, fileIdx)
	}
}

// Context returns the lifecycle context for testing.
func (a *Application) Context() context.Context {
	return a.ctx
}

// --- Stable reload-state accessors ---
//
// These read the *effective* runtime value of reload-affected state through
// whatever field layout Application currently uses. Step 1 of #109 relocated
// the stage pointers (e.g. a.stages.Unpack, a.stages.Repair) into a single
// held builtStages struct; only these accessors needed to change, not the
// tests that call them. See issue #109 (dissolve the Application god object).

// StageStrictSandbox reports the running UnpackStage's strict-sandbox
// setting. Returns false if no unpack stage is configured.
//
// Reads the exported BaseOpts field directly without acquiring
// UnpackStage's internal mutex (unexported, not reachable from this
// package) -- same unlocked-read pattern already used by
// TestApplication_ReloadPostProcOptions_AppliesStrictSandboxToRunningStage
// prior to this change. Fine for single-goroutine test assertions taken
// after the reload call has returned.
func (a *Application) StageStrictSandbox() bool {
	if a.stages.Unpack == nil {
		return false
	}
	return a.stages.Unpack.BaseOpts.Sandbox.Strict
}

// StageUseGoRAR reports the running UnpackStage's pure-Go RAR extraction
// toggle. Returns false if no unpack stage is configured. See
// StageStrictSandbox for the unlocked-read rationale.
func (a *Application) StageUseGoRAR() bool {
	if a.stages.Unpack == nil {
		return false
	}
	return a.stages.Unpack.BaseOpts.UseGoRAR
}

// StageEnableFileJoin reports the running UnpackStage's split-file-join
// toggle. Returns false if no unpack stage is configured. See
// StageStrictSandbox for the unlocked-read rationale.
func (a *Application) StageEnableFileJoin() bool {
	if a.stages.Unpack == nil {
		return false
	}
	return a.stages.Unpack.EnableFileJoin
}

// StagePar2Turbo reports the running RepairStage's par2cmdline-turbo
// toggle. Returns false if no repair stage is configured. See
// StageStrictSandbox for the unlocked-read rationale.
func (a *Application) StagePar2Turbo() bool {
	if a.stages.Repair == nil {
		return false
	}
	return a.stages.Repair.Par2Opts.Turbo
}

// AssemblerMinFreeBytes reports the running Assembler's low-disk-space
// threshold in bytes. Returns 0 if no assembler is configured.
func (a *Application) AssemblerMinFreeBytes() int64 {
	if a.assembler == nil {
		return 0
	}
	return a.assembler.MinFreeBytes()
}

// ForceStopWorkers stops the downloader and assembler without flushing the queue
// or cancelling the application context. Used in scenario tests to simulate an
// abrupt process termination (hard crash) without calling queue.Save() or
// creating shutdown ordering hazards.
//
// noBarrierOnStop is what makes it a hard crash rather than a quiet Shutdown:
// a SIGKILLed process does not get to run a final checkpoint, so neither does
// this. A test that ran one here would be asserting against a clean stop while
// claiming to test a crash.
func (a *Application) ForceStopWorkers() {
	a.stopWorkers(15*time.Second, nil, noBarrierOnStop)
	if a.postProcessor != nil {
		_ = a.postProcessor.Stop()
	}
	if a.dispatcher != nil {
		_ = a.dispatcher.Stop()
	}
}

// BarrierRuns reports how many checkpoint barriers have been started.
//
// The cadence tests need to tell "the barrier fired" from "the barrier had
// nothing to do", and every externally visible effect of a barrier — an ack, a
// committed run — is absent in both cases.
func (a *Application) BarrierRuns() int64 { return a.barrierRuns.Load() }

// Assembler returns the internal assembler for testing.
func (a *Application) Assembler() *assembler.Assembler {
	return a.assembler
}

// NoteJobBytes notes written bytes for a job in the barrier accumulator.
func (a *Application) NoteJobBytes(jobID string, n int) {
	a.noteJobBytes(jobID, n)
}

// JobBarrierState reports whether the job has tracked barrier state (bytes, mutex, or last barrier).
func (a *Application) JobBarrierState(jobID string) (hasBytes bool, hasMu bool, hasLast bool) {
	a.barrierMu.Lock()
	defer a.barrierMu.Unlock()
	_, hasBytes = a.jobBarrierBytes[jobID]
	_, hasMu = a.jobBarrierMu[jobID]
	_, hasLast = a.lastBarrier[jobID]
	return
}

// SetPostProcessorStopHook overrides postProcessor.Stop behavior during testing.
func (a *Application) SetPostProcessorStopHook(fn func() error) {
	a.postProcStopHook = fn
}

// SetShutdownStepTimeout sets the shutdownStepTimeout for testing.
func (a *Application) SetShutdownStepTimeout(d time.Duration) {
	a.shutdownStepTimeout = d
}
