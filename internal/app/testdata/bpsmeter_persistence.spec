pkg ./internal/app/
run TestApplication_BPSMeter

[New does not restore persisted bpsmeter state]
file internal/app/app.go
--- anchor
	if state, err := bpsmeter.LoadState(app.meterStatePath()); err == nil {
		app.meter.Restore(state)
--- replace
	if state, err := bpsmeter.LoadState(app.meterStatePath()); err == nil && false {
		app.meter.Restore(state)
--- end

[New does not log a warning on corrupt bpsmeter state]
file internal/app/app.go
--- anchor
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Warn("load bpsmeter state", "err", err)
	}
--- replace
	} else if false {
		log.Warn("load bpsmeter state", "err", err)
	}
--- end

[New does not wire app.meter into downloader.New]
file internal/app/app.go
--- anchor
		realDL := downloader.New(app.dispatcher, servers, app.meter, app.buildDownloaderOptions(), log)
--- replace
		realDL := downloader.New(app.dispatcher, servers, bpsmeter.NewMeter(10*time.Second, time.Now), app.buildDownloaderOptions(), log)
--- end

[ReloadDownloader does not wire app.meter into downloader.New]
file internal/app/reloader.go
--- anchor
	newDownloader := downloader.New(app.dispatcher, servers, app.meter, app.buildDownloaderOptions(), app.log)
--- replace
	newDownloader := downloader.New(app.dispatcher, servers, nil, app.buildDownloaderOptions(), app.log)
--- end

[Shutdown does not persist app.meter state]
file internal/app/app.go
--- anchor
	if err := bpsmeter.SaveState(app.meterStatePath(), app.meter.Capture()); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end
