pkg ./internal/app/
run ^(TestRunFetch_ReportsACompleteJob|TestRestart_CompleteJobPassesThroughAssessing|TestRetryHistoryJob_CompleteJobPassesThroughAssessing|TestRestart_DropsAJobAlreadyInHistoryBeforeTheResumeSweep|TestReconcileBeforeFirstTick_StopsOnACancelledContext)$
timeout 5m

# How a job that is complete with no download-complete report reaches
# post-processing: its Fetching worker reports it, and it passes through
# Assessing. Also where startup drops a queued job already in history, which
# has to precede anything that routes a job onward.

[the Fetching worker never reports a complete job]
file internal/app/runner.go
--- anchor
	if j.IsComplete() && !r.app.postProcAdmissions.has(j) {
--- replace
	if false {
--- end

[the Fetching worker reports a job already admitted to post-processing]
file internal/app/runner.go
--- anchor
	if j.IsComplete() && !r.app.postProcAdmissions.has(j) {
--- replace
	if j.IsComplete() {
--- end

[the Fetching worker reports an incomplete job]
file internal/app/runner.go
--- anchor
	if j.IsComplete() && !r.app.postProcAdmissions.has(j) {
--- replace
	if !r.app.postProcAdmissions.has(j) {
--- end

[advance reports every verdict from Assessing]
file internal/app/runner.go
--- anchor
	err := r.report.AdvanceFrom(j, from, next)
--- replace
	err := r.report.AdvanceFrom(j, job.Assessing, next)
--- end

[a retry hands a complete job straight to post-processing, as it did before]
file internal/app/app.go
--- anchor
	app.emit(Event{Type: "queue_updated"})
	app.emit(Event{Type: "history_updated"})
	return nil
}
--- replace
	app.emit(Event{Type: "queue_updated"})
	app.emit(Event{Type: "history_updated"})
	if j.IsComplete() {
		app.maybeFinalize(jobID, failMsgForJob(j))
	}
	return nil
}
--- end

[the job already in history is dropped after the resume sweep]
file internal/app/startup_reconcile.go
--- anchor
	if app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: startup reconciliation aborted: %w", err)
			}
			app.dropJobAlreadyInHistory(ctx, row.ID)
		}
	}
	return app.resumeAllJobs(ctx)
--- replace
	if err := app.resumeAllJobs(ctx); err != nil {
		return err
	}
	if app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: startup reconciliation aborted: %w", err)
			}
			app.dropJobAlreadyInHistory(ctx, row.ID)
		}
	}
	return nil
--- end

[a cancelled startup goes on dropping and sweeping jobs]
file internal/app/startup_reconcile.go
--- anchor
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: startup reconciliation aborted: %w", err)
			}
--- replace
			if err := ctx.Err(); err != nil {
				_ = fmt.Errorf("app: startup reconciliation aborted: %w", err)
			}
--- end

[a cancelled startup reaches one more job before it stops]
file internal/app/startup_reconcile.go
--- anchor
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: startup reconciliation aborted: %w", err)
			}
			app.dropJobAlreadyInHistory(ctx, row.ID)
--- replace
			app.dropJobAlreadyInHistory(ctx, row.ID)
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("app: startup reconciliation aborted: %w", err)
			}
--- end

[an application with no dispatcher is reconciled anyway]
file internal/app/startup_reconcile.go
--- anchor
	if app.dispatcher != nil {
		for _, row := range app.dispatcher.List() {
--- replace
	if true {
		for _, row := range app.dispatcher.List() {
--- end
