pkg ./internal/app/
run ^(TestRunFetch_ReportsACompleteJob|TestCompleteFinalizedFile_ReportsFetchingToAssessing|TestCompleteFinalizedFile_AdmittedJobIsNotReportedDownloaded|TestRestart_CompleteJobPassesThroughAssessing|TestRetryHistoryJob_CompleteJobPassesThroughAssessing|TestRestart_DropsAJobAlreadyInHistoryBeforeTheResumeSweep|TestReconcileBeforeFirstTick_StopsOnACancelledContext)$
timeout 5m

# How a job's download-complete report is made: reportDownloadComplete reports
# Fetching -> Assessing for a complete job not admitted to post-processing,
# and both reporters, completeFinalizedFile and a Fetching worker launched on a
# complete job, go through it. Also where startup drops a queued job already in
# history, which has to precede anything that routes a job onward.

[no complete job is ever reported downloaded]
file internal/app/app.go
--- anchor
	if !j.IsComplete() || app.postProcAdmissions.has(j) {
--- replace
	if true {
--- end

[a job admitted to post-processing is reported downloaded]
file internal/app/app.go
--- anchor
	if !j.IsComplete() || app.postProcAdmissions.has(j) {
--- replace
	if !j.IsComplete() {
--- end

[an incomplete job is reported downloaded]
file internal/app/app.go
--- anchor
	if !j.IsComplete() || app.postProcAdmissions.has(j) {
--- replace
	if app.postProcAdmissions.has(j) {
--- end

[the download report names the wrong state it reports from]
file internal/app/app.go
--- anchor
	return true, rep.AdvanceFrom(j, job.Fetching, job.Assessing)
--- replace
	return true, rep.AdvanceFrom(j, job.Assessing, job.Assessing)
--- end

[a file completion reports the download past the owner]
file internal/app/app.go
--- anchor
		if reported, err := app.reportDownloadComplete(j, app.dispatcher); reported {
--- replace
		if reported := j.IsComplete(); reported {
			err := app.dispatcher.AdvanceFrom(j, job.Fetching, job.Assessing)
--- end

[the Fetching worker reports the download past the owner]
file internal/app/runner.go
--- anchor
		if reported, err := r.app.reportDownloadComplete(j, r.report); reported {
--- replace
		if reported := j.IsComplete(); reported {
			err := r.report.AdvanceFrom(j, job.Fetching, job.Assessing)
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
