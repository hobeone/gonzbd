pkg ./internal/app/
run ^(TestStart_LeavesAJobRestoredPausedAtFetchingUnverified|TestLooseRecord_PausedJobReportsEstimateUntilResumed)$

[Start hydrates every paused job before it returns]
file internal/app/app.go
--- anchor
	app.sweepOrphans(app.ctx)
--- replace
	app.sweepOrphans(app.ctx)
	for _, row := range app.dispatcher.List() {
		if j, ok := app.dispatcher.Job(row.ID); ok && j.Intent() == job.IntentPause {
			_ = app.dispatcher.LoadProgress(app.ctx, row.ID)
		}
	}
--- end
