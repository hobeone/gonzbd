pkg ./internal/downloader/
run ^(TestClearEmitted_RefusalIsLoggedAtError|TestEmitResult_DroppedResultRefusedClearIsLoggedAtError|TestApplyDispatchPlan_HopelessFallbackLogsRefusalUnlessGone)$

# A gate that drops a request logs a refused ClearArticleEmitted at Error
# rather than discarding it, and so does a result dropped by cancellation;
# the hopeless fallback's pause logs a refusal unless the job is gone (#286).

[a refused clear is discarded]
file internal/downloader/dispatch.go
--- anchor
	if err := req.job.ClearArticleEmitted(int(req.artIdx)); err != nil {
		d.log.Error("return a gated article to the dispatch pool",
--- replace
	if err := req.job.ClearArticleEmitted(int(req.artIdx)); false && err != nil {
		d.log.Error("return a gated article to the dispatch pool",
--- end

[the hopeless fallback discards a refused pause]
file internal/downloader/dispatch.go
--- anchor
			if err := d.dispatcher.PauseJob(jobID); err != nil && !errors.Is(err, dispatch.ErrNotFound) {
--- replace
			if err := d.dispatcher.PauseJob(jobID); false && err != nil && !errors.Is(err, dispatch.ErrNotFound) {
--- end

[the hopeless fallback inverts its gone classification]
file internal/downloader/dispatch.go
--- anchor
			if err := d.dispatcher.PauseJob(jobID); err != nil && !errors.Is(err, dispatch.ErrNotFound) {
--- replace
			if err := d.dispatcher.PauseJob(jobID); err != nil && errors.Is(err, dispatch.ErrNotFound) {
--- end

[a refused clear for a dropped result is logged below Error]
file internal/downloader/dispatch.go
--- anchor
			d.log.Error("clear the emitted bit for a dropped result",
--- replace
			d.log.Warn("clear the emitted bit for a dropped result",
--- end
