pkg ./internal/app/
run TestReloadDownloader_Keeps

[a built downloader starts unlimited]
file internal/app/app.go
--- anchor
	d.SetSpeedLimit(app.speedLimit)
--- replace
	d.SetSpeedLimit(0)
--- end

[the reload's downloader runs unlimited, as before the fix]
file internal/app/reloader.go
--- anchor
	newDownloader := app.newDownloaderLocked(scs, opts)
--- replace
	newDownloader := app.newDownloaderLocked(scs, opts)
	newDownloader.SetSpeedLimit(0)
--- end

[SetSpeedLimit does not record the limit for later builds]
file internal/app/app.go
--- anchor
	app.speedLimit = bytesPerSec
--- replace
	_ = bytesPerSec
--- end

[New does not seed the limit from the config]
file internal/app/app.go
--- anchor
	app.speedLimit = dl.SpeedLimit()
--- replace
	app.speedLimit = 0
--- end
