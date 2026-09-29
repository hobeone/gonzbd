pkg ./internal/app/
run ^(TestCompleteFinalizedFile_ReportsFetchingToAssessing|TestAppRunner_AdvanceLogsByCause|TestAppRunner_RunAssessBranches)$

# The app's two finished-work reports: the download's Fetching -> Assessing and
# runAssess's verdict, each through Dispatcher.AdvanceFrom.

[the download report names the wrong state it reports from]
file internal/app/app.go
--- anchor
			if err := app.dispatcher.AdvanceFrom(j, job.Fetching, job.Assessing); err != nil && !errors.Is(err, dispatch.ErrStaleReport) {
--- replace
			if err := app.dispatcher.AdvanceFrom(j, job.Assessing, job.Assessing); err != nil && !errors.Is(err, dispatch.ErrStaleReport) {
--- end

[runAssess reports from the wrong state]
file internal/app/runner.go
--- anchor
	err := r.report.AdvanceFrom(j, job.Assessing, next)
--- replace
	err := r.report.AdvanceFrom(j, job.Fetching, next)
--- end

[a repairable verdict is reported as intact]
file internal/app/runner.go
--- anchor
		r.advance(j, job.Repairing)
--- replace
		r.advance(j, job.Extracting)
--- end

[a stale verdict is logged as a failure]
file internal/app/runner.go
--- anchor
	case errors.Is(err, dispatch.ErrStaleReport) || errors.Is(err, dispatch.ErrNotFound):
--- replace
	case errors.Is(err, dispatch.ErrNotFound):
--- end
