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

[newDownloaderLocked does not wire app.meter into downloader.New]
file internal/app/app.go
--- anchor
	d := downloader.New(app.dispatcher, servers, app.meter, opts, app.log)
--- replace
	d := downloader.New(app.dispatcher, servers, bpsmeter.NewMeter(10*time.Second, time.Now), opts, app.log)
--- end

[Shutdown does not persist app.meter state]
file internal/app/app.go
--- anchor
	if err := bpsmeter.SaveState(app.meterStatePath(), app.meter.Capture()); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end
