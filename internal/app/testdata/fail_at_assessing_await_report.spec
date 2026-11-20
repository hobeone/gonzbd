pkg ./internal/app/
run Test(Fail_OnAStalledCompleteJobAtFetching_ResumesItForAssessing|Fail_AtAssessingWithALiveWorker_DefersToTheWorkersExit|FinalizeRegistered_AnEmptyReasonAtAssessing_IsNoFailure|SignalReporter_NotesEachReportAfterForwardingIt)$

[the hopeless verdict's FinishedJob report dropped from failWith]
file internal/app/runner.go
--- anchor
		r.report.FinishedJob(j, job.OutcomeFailed))
--- replace
		nil)
--- end

[the verdict's AdvanceFrom report dropped from advance]
file internal/app/runner.go
--- anchor
	r.logAdvance(j, job.Assessing, next, r.report.AdvanceFrom(j, job.Assessing, next))
--- replace
	r.logAdvance(j, job.Assessing, next, nil)
--- end

[the reporter tee never notes a report]
file internal/app/fail_at_assessing_test.go
--- anchor
	select {
	case r.reported <- struct{}{}:
	default:
	}
--- replace
	_ = r.reported
--- end
