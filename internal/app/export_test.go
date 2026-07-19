// export_test.go exposes internal state for white-box testing.
// This file is compiled only during test builds.
package app

import (
	"context"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/assembler"
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

// TriggerMaybeDirectUnpack calls the unexported maybeDirectUnpack method.
func (a *Application) TriggerMaybeDirectUnpack(fc FileComplete) {
	a.maybeDirectUnpack(fc)
}

// TriggerBuildDirectUnpackOpts calls the unexported buildDirectUnpackOpts method.
func (a *Application) TriggerBuildDirectUnpackOpts() any {
	return a.buildDirectUnpackOpts()
}

// TriggerPersistAndCommit calls the unexported persistAndCommit method.
func (a *Application) TriggerPersistAndCommit(log *slog.Logger, entry history.Entry, job *postproc.Job) error {
	return a.persistAndCommit(log, entry, job)
}

// InjectDirectUnpacker injects a direct unpacker for testing.
func (a *Application) InjectDirectUnpacker(jobID string, du *directunpack.DirectUnpacker) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.directUnpackers[jobID] = du
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

// SetActiveDU sets the activeDU count.
func (a *Application) SetActiveDU(val int32) {
	a.activeDU.Store(val)
}

// TriggerFireCompletionNotification calls the unexported fireCompletionNotification method.
func (a *Application) TriggerFireCompletionNotification(entry history.Entry) {
	a.fireCompletionNotification(entry)
}
