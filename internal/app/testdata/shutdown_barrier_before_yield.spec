pkg ./internal/app/
run TestStopWorkers_TheShutdownBarrierCoversAJobTheYieldWouldEvict$

[the clean-shutdown barrier run after the Fetching yield]
file internal/app/app.go
--- anchor
	if barrier {
		app.shutdownCheckpoint()
	}

	// If dl.Stop returned cleanly with no error, all downloader workers have definitely
	// exited and will not touch manifests or barriers again. Yield Fetching jobs so
	// Dispatcher.Stop can cleanly park and evict. If dl.Stop timed out, do NOT yield,
	// so Dispatcher.Stop observes wait worker timeout and skips eviction.
	if dl != nil && dlErr == nil && app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if row.View.State == job.Fetching {
				_ = app.dispatcher.Yielded(row.ID)
			}
		}
	}
--- replace
	if dl != nil && dlErr == nil && app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if row.View.State == job.Fetching {
				_ = app.dispatcher.Yielded(row.ID)
			}
		}
	}
	if barrier {
		app.shutdownCheckpoint()
	}
--- end

[the Fetching yield loop never runs, so the barrier's coverage is never handed off]
file internal/app/app.go
--- anchor
	if dl != nil && dlErr == nil && app.dispatcher != nil {
--- replace
	if false && dl != nil && dlErr == nil && app.dispatcher != nil {
--- end
