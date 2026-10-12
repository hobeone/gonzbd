pkg ./internal/app/
run ^(TestUnlessGone|TestWarnUnlessGone|TestAppRunner_RefusedReportIsLoggedUnlessGone|TestPipelineClearEmitted_RefusalIsLoggedAtError|TestFileOwedUnwantedFailures_FailedHydrationIsLoggedAndFiled|TestYieldStoppedWorkers_YieldsOnlyTheNamedStates|TestYieldStoppedWorkers_CollectsARefusal|TestStopWorkers_YieldsFetchingJobs|TestStall_RefusedPauseIsLogged|TestStall_PauseRefusedByARemovalIsNotLogged|TestRemoveJob_RefusedCancelIsLogged|TestApplication_ShutdownWithOptions|TestShutdown_PropagatesErrors|TestApplication_Shutdown_WedgedComponent|TestStopAndJoin_WaitsForWgBeforePostProcAndReportsTimeout|TestCheckpoint_SurvivesCrashMidPostProc|TestRestart_CompleteJobPassesThroughAssessing)$

# Dispatcher refusals and ClearArticleEmitted refusals that used to be
# discarded (#286): a refusal for a job that is gone is dropped, any other is
# logged or returned.

[a job that is gone is treated as a failure]
file internal/app/dispatch_refusal.go
--- anchor
	if errors.Is(err, dispatch.ErrNotFound) {
--- replace
	if errors.Is(err, dispatch.ErrNotFound) && false {
--- end

[every refusal is dropped]
file internal/app/dispatch_refusal.go
--- anchor
		return nil
	}
	return err
--- replace
		return nil
	}
	return nil
--- end

[warnUnlessGone warns about a job that is gone]
file internal/app/dispatch_refusal.go
--- anchor
	if err = unlessGone(err); err == nil {
--- replace
	if err == nil {
--- end

[the runner discards a refused yield]
file internal/app/runner.go
--- anchor
	warnUnlessGone(r.log, "runner: yield failed", id, r.report.Yielded(id))
--- replace
	_ = r.report.Yielded(id)
--- end

[the runner discards a refused settle]
file internal/app/runner.go
--- anchor
		warnUnlessGone(r.log, "runner: settling the job failed", j.ID(),
			r.report.FinishedJob(j, job.OutcomeFailed))
--- replace
		_ = r.report.FinishedJob(j, job.OutcomeFailed)
--- end

[the pipeline discards a refused clear]
file internal/app/pipeline.go
--- anchor
	if err := res.Job.ClearArticleEmitted(int(res.ArtIdx)); err != nil {
--- replace
	if err := res.Job.ClearArticleEmitted(int(res.ArtIdx)); false && err != nil {
--- end

[a failed hydration of an owed job is discarded]
file internal/app/startup_reconcile.go
--- anchor
			if err := app.residency.Hydrate(ctx, row.ID); err != nil {
--- replace
			if err := app.residency.Hydrate(ctx, row.ID); false && err != nil {
--- end

[a failed hydration skips the filing]
file internal/app/startup_reconcile.go
--- anchor
					"job", row.ID, "err", err)
			}
--- replace
					"job", row.ID, "err", err)
				continue
			}
--- end

[the shutdown yield inverts its state filter]
file internal/app/app.go
--- anchor
		if !slices.Contains(states, row.View.State) {
--- replace
		if slices.Contains(states, row.View.State) {
--- end

[a refused shutdown yield is not collected]
file internal/app/app.go
--- anchor
		if err := unlessGone(app.dispatcher.Yielded(row.ID)); err != nil && errs != nil {
--- replace
		if err := unlessGone(app.dispatcher.Yielded(row.ID)); err != nil && false {
--- end

[stopWorkers yields the post-processing states instead of Fetching]
file internal/app/app.go
--- anchor
		app.yieldStoppedWorkers(errs, job.Fetching)
--- replace
		app.yieldStoppedWorkers(errs, job.Repairing, job.Extracting, job.Finalizing)
--- end

[joinAndStop yields Fetching instead of the post-processing states]
file internal/app/app.go
--- anchor
		app.yieldStoppedWorkers(errs, job.Repairing, job.Extracting, job.Finalizing)
--- replace
		app.yieldStoppedWorkers(errs, job.Fetching)
--- end

[stall discards a refused pause]
file internal/app/durability.go
--- anchor
		if err := app.dispatcher.PauseJob(jobID); !errors.Is(err, job.ErrIntentLatched) {
--- replace
		if err := app.dispatcher.PauseJob(jobID); false && !errors.Is(err, job.ErrIntentLatched) {
--- end

[stall warns about a pause refused by a removal's latched cancel]
file internal/app/durability.go
--- anchor
		if err := app.dispatcher.PauseJob(jobID); !errors.Is(err, job.ErrIntentLatched) {
--- replace
		if err := app.dispatcher.PauseJob(jobID); err != nil || !errors.Is(err, job.ErrIntentLatched) {
--- end

[RemoveJob discards a refused cancel]
file internal/app/app.go
--- anchor
	warnUnlessGone(app.log, "remove: cancelling the job failed", id, app.dispatcher.Cancel(id))
--- replace
	_ = app.dispatcher.Cancel(id)
--- end
