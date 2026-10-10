pkg ./internal/app/
run ^TestResume_ReevaluatesStalls$

[a queue-wide resume does not re-evaluate stalls (#792)]
file internal/app/app.go
--- anchor
		app.downloader.Resume()
	}
	app.ReevaluateStalls()
}
--- replace
		app.downloader.Resume()
	}
}
--- end

[the low-disk auto-resume bypasses resumeLocked (#792)]
file internal/app/app.go
--- anchor
		return true
	}
	app.resumeLocked()
	app.mu.Unlock()
--- replace
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
--- end

[ResumeDownloads bypasses resumeLocked]
file internal/app/app.go
--- anchor
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
	app.resumeLocked()
--- replace
func (app *Application) ResumeDownloads() {
	app.mu.Lock()
	app.pauseReason = pauseReasonNone
	app.stopLowDiskWatchLocked()
	if app.dispatcher != nil {
		app.dispatcher.Resume()
	}
	if app.downloader != nil {
		app.downloader.Resume()
	}
--- end
