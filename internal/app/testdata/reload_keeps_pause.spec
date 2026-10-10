pkg ./internal/app/
run ^TestReloadDownloader_KeepsAPause$

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
