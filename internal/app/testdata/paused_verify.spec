pkg ./internal/app/
run TestVerifyPausedJobs_StartReturnsBeforeTheLoadEnds|TestVerifyPausedJobs_OwedFilingIsNotDeferredBehindIt|TestVerifyPausedJobs_LoadsOnlyJobsPausedAtFetching|TestVerifyPausedJobs_DeadlineLeavesTheJobUnloaded|TestVerifyPausedJobs_StopsAtShutdown|TestVerifyPausedJobs_ShutdownJoinsTheVerifier|TestVerifyPausedJobs_RemoveDuringTheLoad|TestLooseRecord_PausedJobReportsVerifiedProgress

[the paused jobs are verified on Start's path]
file internal/app/app.go
--- anchor
	app.wg.Go(func() { app.verifyPausedJobs(app.ctx) })
--- replace
	app.verifyPausedJobs(app.ctx)
--- end

[the verifier is not on app.wg]
file internal/app/app.go
--- anchor
	app.wg.Go(func() { app.verifyPausedJobs(app.ctx) })
--- replace
	go app.verifyPausedJobs(app.ctx)
--- end

[the paused jobs are never loaded]
file internal/app/startup_reconcile.go
--- anchor
		err := app.dispatcher.LoadProgress(jctx, row.ID)
--- replace
		err := app.dispatcher.LoadProgress(jctx, "")
--- end

[a job paused before it ran is loaded]
file internal/app/startup_reconcile.go
--- anchor
		if !ok || row.View.State != job.Fetching || j.Intent() != job.IntentPause {
--- replace
		if !ok || j.Intent() != job.IntentPause {
--- end

[a running job at Fetching is loaded]
file internal/app/startup_reconcile.go
--- anchor
		if !ok || row.View.State != job.Fetching || j.Intent() != job.IntentPause {
--- replace
		if !ok || row.View.State != job.Fetching || j.Intent() == job.IntentCancel {
--- end

[the owed filing waits behind the paused jobs' verification]
file internal/app/startup_reconcile.go
--- anchor
	return app.fileOwedUnwantedFailures(ctx)
}
--- replace
	app.wg.Go(func() { app.verifyPausedJobs(app.ctx); _ = app.fileOwedUnwantedFailures(app.ctx) })
	return nil
}
--- end

[a load has no deadline]
file internal/app/startup_reconcile.go
--- anchor
		jctx, cancel := context.WithTimeout(ctx, app.pausedVerifyTimeout)
--- replace
		jctx, cancel := context.WithCancel(ctx)
--- end

[a load outlives the app's context]
file internal/app/startup_reconcile.go
--- anchor
		jctx, cancel := context.WithTimeout(ctx, app.pausedVerifyTimeout)
--- replace
		jctx, cancel := context.WithTimeout(context.Background(), app.pausedVerifyTimeout)
--- end

[the verifier goes on after its context ended]
file internal/app/startup_reconcile.go
--- anchor
		if ctx.Err() != nil {
			app.log.Info("paused-job verification stopped; the app is stopping", "job", row.ID)
--- replace
		if false {
			app.log.Info("paused-job verification stopped; the app is stopping", "job", row.ID)
--- end

[no queue_updated names a job whose load landed]
file internal/app/startup_reconcile.go
--- anchor
		app.emit(Event{Type: "queue_updated", NzoID: row.ID})
--- replace
		_ = row.ID
--- end

[queue_updated names a job whose load did not land]
file internal/app/startup_reconcile.go
--- anchor
				"job", row.ID, "timeout", app.pausedVerifyTimeout, "err", err)
			continue
--- replace
				"job", row.ID, "timeout", app.pausedVerifyTimeout, "err", err)
--- end

[a removal does not wait for the hydration in flight]
file internal/app/residency.go
--- anchor
	if ready, ok := r.hydrating[id]; ok {
		r.mu.Unlock()
		<-ready
		r.mu.Lock()
	}
--- replace
	if _, ok := r.hydrating[id]; ok && false {
		r.mu.Unlock()
		r.mu.Lock()
	}
--- end
