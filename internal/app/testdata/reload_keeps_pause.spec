pkg ./internal/app/
run ^(TestReloadDownloader_KeepsAPause|TestReloadDownloader_PauseOrResumeDuringReloadReachesTheNewDownloader)$

[ReloadDownloader starts its new downloader unpaused while the application is paused (#791)]
file internal/app/reloader.go
--- anchor
	if app.pauseReason != pauseReasonNone {
		newDownloader.Pause()
	}
--- replace
	if false {
		newDownloader.Pause()
	}
--- end

[ReloadDownloader drops app.mu between the pause decision and the swap]
file internal/app/reloader.go
--- anchor
	app.mu.Lock()
	if app.pauseReason != pauseReasonNone {
		newDownloader.Pause()
	}
	if app.reloadBeforeStartHook != nil {
		app.reloadBeforeStartHook(newDownloader)
	}
	if err := newDownloader.Start(app.ctx); err != nil {
		app.mu.Unlock()
		return err
	}
	app.downloader = newDownloader
--- replace
	app.mu.Lock()
	paused := app.pauseReason != pauseReasonNone
	hook := app.reloadBeforeStartHook
	app.mu.Unlock()
	if paused {
		newDownloader.Pause()
	}
	if hook != nil {
		hook(newDownloader)
	}
	if err := newDownloader.Start(app.ctx); err != nil {
		return err
	}
	app.mu.Lock()
	app.downloader = newDownloader
--- end

[ReloadDownloader pauses the new downloader only after Start]
file internal/app/reloader.go
--- anchor
	if app.pauseReason != pauseReasonNone {
		newDownloader.Pause()
	}
	if app.reloadBeforeStartHook != nil {
		app.reloadBeforeStartHook(newDownloader)
	}
	if err := newDownloader.Start(app.ctx); err != nil {
		app.mu.Unlock()
		return err
	}
--- replace
	if app.reloadBeforeStartHook != nil {
		app.reloadBeforeStartHook(newDownloader)
	}
	if err := newDownloader.Start(app.ctx); err != nil {
		app.mu.Unlock()
		return err
	}
	if app.pauseReason != pauseReasonNone {
		newDownloader.Pause()
	}
--- end
