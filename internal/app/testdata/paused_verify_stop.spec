pkg ./internal/app/
run ^TestVerifyPausedJobs_StopsAtTheFirstLoadOnceTheAppIsStopping$

# The startup verifier of jobs paused at Fetching stops at the first load once
# its context is done, rather than walking the rest of the queue.

[a stopping app walks on to the next paused job]
file internal/app/startup_reconcile.go
--- anchor
		if ctx.Err() != nil {
			app.log.Info("paused-job verification stopped; the app is stopping", "job", row.ID)
			return
--- replace
		if ctx.Err() != nil {
			app.log.Info("paused-job verification stopped; the app is stopping", "job", row.ID)
			continue
--- end
