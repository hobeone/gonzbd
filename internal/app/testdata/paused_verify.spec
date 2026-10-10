pkg ./internal/app/
run TestVerifyPausedJobs_StartReturnsBeforeTheLoadEnds|TestVerifyPausedJobs_OwedFilingIsNotDeferredBehindIt|TestVerifyPausedJobs_DeadlineLeavesTheJobUnloaded|TestVerifyPausedJobs_StopsAtShutdown|TestVerifyPausedJobs_ShutdownJoinsTheVerifier|TestLooseRecord_PausedJobReportsVerifiedProgress

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
