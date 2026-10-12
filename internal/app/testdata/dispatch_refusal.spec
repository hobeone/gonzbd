pkg ./internal/app/
run ^(TestUnlessGone|TestWarnUnlessGone|TestAppRunner_RefusedReportIsLoggedUnlessGone|TestPipelineClearEmitted_RefusalIsLoggedAtError|TestFileOwedUnwantedFailures_FailedHydrationIsLoggedAndFiled)$

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
