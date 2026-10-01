pkg ./internal/app/
run ^(TestAppRunner_AdvanceLogsByCause|TestAppRunner_RunAssessBranches)$

# runAssess's verdict, reported through Dispatcher.AdvanceFrom. The download's
# Fetching -> Assessing report (reportDownloadComplete) is
# download_complete_route.spec's.

[runAssess reports from the wrong state]
file internal/app/runner.go
--- anchor
	r.logAdvance(j, job.Assessing, next, r.report.AdvanceFrom(j, job.Assessing, next))
--- replace
	r.logAdvance(j, job.Assessing, next, r.report.AdvanceFrom(j, job.Fetching, next))
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
