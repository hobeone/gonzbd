// export_test.go exposes internal state for white-box testing.
// This file is compiled only during test builds.
package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/assembler"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/directunpack"
	"github.com/hobeone/gonzbd/internal/downloader"
	"github.com/hobeone/gonzbd/internal/durability"
	"github.com/hobeone/gonzbd/internal/history"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/postproc"
)

// ErrFinalizedJobRemoved exposes errFinalizedJobRemoved to the external test
// package.
var ErrFinalizedJobRemoved = errFinalizedJobRemoved

// ErrJobInTransition exposes errJobInTransition to the external test package.
var ErrJobInTransition = errJobInTransition

// ErrRetryDirConflict exposes errRetryDirConflict to the external test package.
var ErrRetryDirConflict = errRetryDirConflict

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

// TriggerMaybeDirectUnpack drives the DirectUnpack orchestrator's start path.
func (a *Application) TriggerMaybeDirectUnpack(fc FileComplete) {
	a.duOrch.maybeStart(fc)
}

// TriggerBuildDirectUnpackOpts calls the orchestrator's option builder.
func (a *Application) TriggerBuildDirectUnpackOpts() any {
	return a.duOrch.buildOpts(false, false, false, 0)
}

// TriggerPersistAndCommit calls the finalizer's persistAndCommit method.
func (a *Application) TriggerPersistAndCommit(log *slog.Logger, entry history.Entry, job *postproc.Job) error {
	return a.finalizer.persistAndCommit(log, entry, job)
}

// DispatcherIsOccupied reports whether the dispatcher has active occupiers for id.
func (a *Application) DispatcherIsOccupied(id string) bool {
	if a.dispatcher == nil {
		return false
	}
	return a.dispatcher.IsOccupied(id)
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

// stopAndJoin performs a hard-crash teardown in Shutdown's component order
// (state guards -> stopWorkers -> joinAndStop) without the recorder's final
// flush, so the record holds only what an explicit or periodic flush wrote.
func (a *Application) stopAndJoin() error {
	if !a.stopped.CompareAndSwap(false, true) {
		return nil
	}
	a.stopping.Store(true)
	if a.dispatcher != nil {
		a.dispatcher.Pause()
	}

	stepTimeout := min(a.stepTimeout(), 5*time.Second)
	var errs []error
	a.stopWorkers(stepTimeout, &errs)
	a.joinAndStop(stepTimeout, &errs)
	return errors.Join(errs...)
}

// ForceStopWorkers stops the downloader and assembler, cancels the application
// context,
// waits for background goroutines on wg to exit, and stops the post-processor
// and dispatcher. Fails tb if any teardown step times out or errors. Used in
// scenario tests to simulate an abrupt process termination (hard crash) without
// calling Shutdown().
//
// Skipping the recorder's final flush is what makes it a hard crash rather than
// a quiet Shutdown: a SIGKILLed process does not get to flush, so neither does
// this.
func (a *Application) ForceStopWorkers(tb testing.TB) {
	tb.Helper()
	if err := a.stopAndJoin(); err != nil {
		tb.Fatalf("ForceStopWorkers: %v", err)
	}
}

// StopAndJoin runs ForceStopWorkers' hard-crash teardown and fails tb if any
// teardown step (including waiting for background goroutines on wg) times out
// or errors. Used in test cleanups so that background loops do not hold or
// create files while t.TempDir() is being removed.
func (a *Application) StopAndJoin(tb testing.TB) {
	tb.Helper()
	if err := a.stopAndJoin(); err != nil {
		tb.Errorf("StopAndJoin: %v", err)
	}
}

// Assembler returns the internal assembler for testing.
func (a *Application) Assembler() *assembler.Assembler {
	return a.assembler
}

// PostProcessorHas reports whether post-processing holds the job instance
// registered under jobID, queued or in flight.
func (a *Application) PostProcessorHas(jobID string) bool {
	j, ok := a.dispatcher.Job(jobID)
	return ok && a.postProcessor.HasJob(j)
}

// SetPostProcessorStopHook overrides postProcessor.Stop behavior during testing.
func (a *Application) SetPostProcessorStopHook(fn func() error) {
	a.postProcStopHook = fn
}

// SetShutdownStepTimeout sets the shutdownStepTimeout for testing.
func (a *Application) SetShutdownStepTimeout(d time.Duration) {
	a.shutdownStepTimeout = d
}

// ErrRecordForJob is the error a store installed by FailRecordFor returns.
var ErrRecordForJob = errors.New("test: record write failed")

// AwaitHydration returns once no hydration of id is in flight. A job turns
// Resident when its content is attached, which is before the verified rows
// are installed; a Hydrate call made meanwhile waits for the one in flight.
func (a *Application) AwaitHydration(ctx context.Context, id string) error {
	return a.residency.Hydrate(ctx, id)
}

// FailRecordFor makes every record write carrying a batch for jobID fail with
// ErrRecordForJob. Call it before Start.
func (a *Application) FailRecordFor(jobID string) {
	a.recorder.st = failRecordFor{recordStore: a.recorder.st, id: jobID}
}

type failRecordFor struct {
	recordStore
	id string
}

func (s failRecordFor) ApplyRecord(ctx context.Context, batches []durability.RecordBatch) error {
	for _, b := range batches {
		if b.JobID == s.id {
			return ErrRecordForJob
		}
	}
	return s.recordStore.ApplyRecord(ctx, batches)
}

// SeedWritten is seedWritten for the external test package.
func SeedWritten(t *testing.T, st *durability.Store, jobID string, rows []durability.WrittenRow) {
	t.Helper()
	seedWritten(t, st, jobID, rows)
}

// WriteJobManifest persists a job's manifest the way AddJob does.
//
// A test that puts a job in through Dispatcher().Add bypasses AddJob, and so
// bypasses this write — see writeJobManifest's doc for what the next tick then
// does to the job.
func WriteJobManifest(adminDir string, j *job.Job) error {
	return writeJobManifest(adminDir, j)
}

// ManifestPath is manifestPath for the external test package, so a test can
// name the file without repeating the layout manifestpath.go owns.
func ManifestPath(adminDir, jobID string) (string, error) {
	return manifestPath(adminDir, jobID)
}

// SetAssessHook installs assessHook for the external test package. Call it
// before Start.
func (a *Application) SetAssessHook(fn func(id string)) { a.assessHook = fn }
