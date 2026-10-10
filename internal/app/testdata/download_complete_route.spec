pkg ./internal/app/
run ^(TestRunFetch_ReportsACompleteJob|TestCompleteFinalizedFile_ReportsFetchingToAssessing|TestCompleteFinalizedFile_AdmittedJobIsNotReportedDownloaded|TestRetryHistoryJob_CompleteJobPassesThroughAssessing|TestRestart_DropsAJobAlreadyInHistoryBeforeItIsTicked|TestReconcileBeforeFirstTick_StopsOnACancelledContext)$
timeout 1m

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

[the Assessing worker settles its verdict Failed]
file internal/app/runner.go
--- anchor
		if reasons := r.app.postProcAdmissions.takeDeferred(j); len(reasons) > 0 {
--- replace
		if reasons := r.app.postProcAdmissions.takeDeferred(j); true {
--- end

[the job already in history is not dropped before the first tick]
file internal/app/startup_reconcile.go
--- anchor
			app.dropJobAlreadyInHistory(ctx, row.ID)
		}
	}
	return app.fileOwedUnwantedFailures(ctx)
--- replace
			_ = row.ID
		}
	}
	return app.fileOwedUnwantedFailures(ctx)
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
