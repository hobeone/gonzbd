pkg ./internal/app/
run ^(TestLowDiskPause_UnifiesQueuePauseAndAutoResumes|TestLowDiskPause_UserPauseOverridesAutoResume|TestTryAutoResumeLowDisk_DirectBranches)$

[PauseDownloads does not record pauseReasonUser]
file internal/app/app.go
--- anchor
func (app *Application) PauseDownloads() {
	app.mu.Lock()
	app.pauseReason = pauseReasonUser
--- replace
func (app *Application) PauseDownloads() {
	app.mu.Lock()
	app.pauseReason = pauseReasonLowDisk
--- end

[PauseDownloads does not cancel an active low-disk watch]
file internal/app/app.go
--- anchor
	app.pauseReason = pauseReasonUser
	app.stopLowDiskWatchLocked()
--- replace
	app.pauseReason = pauseReasonUser
--- end

[PauseDownloads does not pause the dispatcher]
file internal/app/app.go
--- anchor
	app.pauseReason = pauseReasonUser
	app.stopLowDiskWatchLocked()
	if app.dispatcher != nil {
		app.dispatcher.Pause()
	}
--- replace
	app.pauseReason = pauseReasonUser
	app.stopLowDiskWatchLocked()
	if false && app.dispatcher != nil {
		app.dispatcher.Pause()
	}
--- end

[ResumeDownloads does not clear pauseReason]
file internal/app/app.go
--- anchor
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
	app.pauseReason = pauseReasonNone
--- replace
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
--- end

[ResumeDownloads does not cancel an active low-disk watch]
file internal/app/app.go
--- anchor
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
--- replace
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
	app.pauseReason = pauseReasonNone
--- end

[ResumeDownloads does not resume the dispatcher]
file internal/app/app.go
--- anchor
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if app.dispatcher != nil {
		app.dispatcher.Resume()
	}
--- replace
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if false && app.dispatcher != nil {
		app.dispatcher.Resume()
	}
--- end

[handleLowDisk overwrites a user pause with pauseReasonLowDisk]
file internal/app/app.go
--- anchor
	if app.pauseReason != pauseReasonUser {
		app.pauseReason = pauseReasonLowDisk
	}
--- replace
	if true {
		app.pauseReason = pauseReasonLowDisk
	}
--- end

[handleLowDisk does not record pauseReasonLowDisk]
file internal/app/app.go
--- anchor
	if app.pauseReason != pauseReasonUser {
		app.pauseReason = pauseReasonLowDisk
	}
--- replace
	if false {
		app.pauseReason = pauseReasonLowDisk
	}
--- end

[handleLowDisk does not pause the dispatcher]
file internal/app/app.go
--- anchor
	if app.pauseReason != pauseReasonUser {
		app.pauseReason = pauseReasonLowDisk
	}
	if app.dispatcher != nil {
		app.dispatcher.Pause()
	}
--- replace
	if app.pauseReason != pauseReasonUser {
		app.pauseReason = pauseReasonLowDisk
	}
	if false && app.dispatcher != nil {
		app.dispatcher.Pause()
	}
--- end

[handleLowDisk does not pause the downloader]
file internal/app/app.go
--- anchor
	if app.pauseReason != pauseReasonUser {
		app.pauseReason = pauseReasonLowDisk
	}
	if app.dispatcher != nil {
		app.dispatcher.Pause()
	}
	if app.downloader != nil {
		app.downloader.Pause()
	}
--- replace
	if app.pauseReason != pauseReasonUser {
		app.pauseReason = pauseReasonLowDisk
	}
	if app.dispatcher != nil {
		app.dispatcher.Pause()
	}
	if false && app.downloader != nil {
		app.downloader.Pause()
	}
--- end

[handleLowDisk pauses the downloader outside app.mu]
file internal/app/app.go
--- anchor
	if app.downloader != nil {
		app.downloader.Pause()
	}
	if app.pauseReason == pauseReasonLowDisk && !app.stopping.Load() && !app.stopped.Load() && app.lowDiskRecheckInterval > 0 && app.lowDiskCancel == nil {
		watchCtx, cancel := context.WithCancel(app.ctx)
		app.lowDiskCancel = cancel
		interval := app.lowDiskRecheckInterval
		app.lowDiskWg.Go(func() {
			app.watchLowDisk(watchCtx, dir, interval)
		})
	}
	app.mu.Unlock()
--- replace
	dl := app.downloader
	if app.pauseReason == pauseReasonLowDisk && !app.stopping.Load() && !app.stopped.Load() && app.lowDiskRecheckInterval > 0 && app.lowDiskCancel == nil {
		watchCtx, cancel := context.WithCancel(app.ctx)
		app.lowDiskCancel = cancel
		interval := app.lowDiskRecheckInterval
		app.lowDiskWg.Go(func() {
			app.watchLowDisk(watchCtx, dir, interval)
		})
	}
	app.mu.Unlock()
	if dl != nil {
		dl.Pause()
	}
--- end

[handleLowDisk spawns a duplicate watcher when lowDiskCancel is already set]
file internal/app/app.go
--- anchor
	if app.pauseReason == pauseReasonLowDisk && !app.stopping.Load() && !app.stopped.Load() && app.lowDiskRecheckInterval > 0 && app.lowDiskCancel == nil {
--- replace
	if app.pauseReason == pauseReasonLowDisk && !app.stopping.Load() && !app.stopped.Load() && app.lowDiskRecheckInterval > 0 {
--- end

[handleLowDisk starts a watcher even while stopping]
file internal/app/app.go
--- anchor
	if app.pauseReason == pauseReasonLowDisk && !app.stopping.Load() && !app.stopped.Load() && app.lowDiskRecheckInterval > 0 && app.lowDiskCancel == nil {
--- replace
	if app.pauseReason == pauseReasonLowDisk && !app.stopped.Load() && app.lowDiskRecheckInterval > 0 && app.lowDiskCancel == nil {
--- end

[handleLowDisk starts a watcher even when stopped]
file internal/app/app.go
--- anchor
	if app.pauseReason == pauseReasonLowDisk && !app.stopping.Load() && !app.stopped.Load() && app.lowDiskRecheckInterval > 0 && app.lowDiskCancel == nil {
--- replace
	if app.pauseReason == pauseReasonLowDisk && !app.stopping.Load() && app.lowDiskRecheckInterval > 0 && app.lowDiskCancel == nil {
--- end

[handleLowDisk does not broadcast queue_updated]
file internal/app/app.go
--- anchor
	app.log.Warn("low disk space, downloads paused",
		"dir", dir,
		"freeMB", freeBytes/(1024*1024))
	app.emit(Event{Type: "queue_updated"})
--- replace
	app.log.Warn("low disk space, downloads paused",
		"dir", dir,
		"freeMB", freeBytes/(1024*1024))
--- end

[stopWorkers does not stop the low-disk watch]
file internal/app/app.go
--- anchor
func (app *Application) stopWorkers(stepTimeout time.Duration, errs *[]error, barrier finalBarrier) {
	app.stopLowDiskWatch()
--- replace
func (app *Application) stopWorkers(stepTimeout time.Duration, errs *[]error, barrier finalBarrier) {
--- end

[stopLowDiskWatch does not wait on lowDiskWg]
file internal/app/app.go
--- anchor
	app.stopLowDiskWatchLocked()
	app.mu.Unlock()
	app.lowDiskWg.Wait()
}
--- replace
	app.stopLowDiskWatchLocked()
	app.mu.Unlock()
}
--- end

[tryAutoResumeLowDisk does not fall back to downloadDir when job subdir was deleted]
file internal/app/app.go
--- anchor
	if errors.Is(err, os.ErrNotExist) {
		if dlDir := app.downloadDir(); dlDir != "" && dlDir != probeDir {
--- replace
	if false && errors.Is(err, os.ErrNotExist) {
		if dlDir := app.downloadDir(); dlDir != "" && dlDir != probeDir {
--- end

[tryAutoResumeLowDisk ignores a probe error and resumes anyway]
file internal/app/app.go
--- anchor
	if err != nil {
		app.log.Warn("low-disk auto-resume check failed", "dir", probeDir, "err", err)
		return false
	}
--- replace
	if false && err != nil {
		app.log.Warn("low-disk auto-resume check failed", "dir", probeDir, "err", err)
		return false
	}
--- end

[tryAutoResumeLowDisk uses strict > instead of >= at the MinFreeBytes boundary]
file internal/app/app.go
--- anchor
	if free < app.assembler.MinFreeBytes() {
		return false
	}
--- replace
	if free <= app.assembler.MinFreeBytes() {
		return false
	}
--- end

[tryAutoResumeLowDisk resumes even while free space is below MinFreeBytes]
file internal/app/app.go
--- anchor
	if free < app.assembler.MinFreeBytes() {
		return false
	}
--- replace
	if false && free < app.assembler.MinFreeBytes() {
		return false
	}
--- end

[tryAutoResumeLowDisk resumes even when pauseReason is no longer pauseReasonLowDisk]
file internal/app/app.go
--- anchor
	if app.pauseReason != pauseReasonLowDisk || app.stopping.Load() || app.stopped.Load() {
		app.mu.Unlock()
		return true
	}
--- replace
	if app.stopping.Load() || app.stopped.Load() {
		app.mu.Unlock()
		return true
	}
--- end

[tryAutoResumeLowDisk resumes even while stopping]
file internal/app/app.go
--- anchor
	if app.pauseReason != pauseReasonLowDisk || app.stopping.Load() || app.stopped.Load() {
		app.mu.Unlock()
		return true
	}
--- replace
	if app.pauseReason != pauseReasonLowDisk || app.stopped.Load() {
		app.mu.Unlock()
		return true
	}
--- end

[tryAutoResumeLowDisk resumes even when stopped]
file internal/app/app.go
--- anchor
	if app.pauseReason != pauseReasonLowDisk || app.stopping.Load() || app.stopped.Load() {
		app.mu.Unlock()
		return true
	}
--- replace
	if app.pauseReason != pauseReasonLowDisk || app.stopping.Load() {
		app.mu.Unlock()
		return true
	}
--- end

[tryAutoResumeLowDisk does not clear lowDiskCancel on auto-resume]
file internal/app/app.go
--- anchor
		return true
	}
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
--- replace
		return true
	}
	app.pauseReason = pauseReasonNone
--- end

[tryAutoResumeLowDisk does not resume the dispatcher]
file internal/app/app.go
--- anchor
		return true
	}
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if app.dispatcher != nil {
		app.dispatcher.Resume()
	}
--- replace
		return true
	}
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if false && app.dispatcher != nil {
		app.dispatcher.Resume()
	}
--- end

[tryAutoResumeLowDisk does not resume the downloader]
file internal/app/app.go
--- anchor
		return true
	}
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if app.dispatcher != nil {
		app.dispatcher.Resume()
	}
	if app.downloader != nil {
		app.downloader.Resume()
	}
	app.mu.Unlock()
--- replace
		return true
	}
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if app.dispatcher != nil {
		app.dispatcher.Resume()
	}
	if false && app.downloader != nil {
		app.downloader.Resume()
	}
	app.mu.Unlock()
--- end

[tryAutoResumeLowDisk resumes the downloader outside app.mu]
file internal/app/app.go
--- anchor
		return true
	}
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if app.dispatcher != nil {
		app.dispatcher.Resume()
	}
	if app.downloader != nil {
		app.downloader.Resume()
	}
	app.mu.Unlock()
--- replace
		return true
	}
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if app.dispatcher != nil {
		app.dispatcher.Resume()
	}
	dl := app.downloader
	app.mu.Unlock()
	if dl != nil {
		dl.Resume()
	}
--- end

[tryAutoResumeLowDisk does not broadcast queue_updated]
file internal/app/app.go
--- anchor
	app.log.Info("disk space recovered, downloads auto-resumed",
		"dir", probeDir,
		"freeMB", free/(1024*1024))
	app.emit(Event{Type: "queue_updated"})
--- replace
	app.log.Info("disk space recovered, downloads auto-resumed",
		"dir", probeDir,
		"freeMB", free/(1024*1024))
--- end
