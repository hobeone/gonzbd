pkg ./internal/downloader/
run ^TestStart_KeepsAPauseMadeBeforeIt$

[Start re-opens the fetch context of a downloader paused before it]
file internal/downloader/downloader.go
--- anchor
	if d.paused.Load() {
		d.pauseCancel()
	}
	d.pauseMu.Unlock()
--- replace
	if false {
		d.pauseCancel()
	}
	d.pauseMu.Unlock()
--- end
