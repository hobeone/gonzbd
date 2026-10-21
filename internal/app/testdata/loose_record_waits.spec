pkg ./internal/app/
run ^(TestLooseRecord_VerificationFaultParksNeverFails|TestLooseRecord_PausedJobReportsVerifiedProgress|TestLooseRecord_UntrustedSurvivesEviction|TestLooseRecord_CompletionFaultConvergesInProcess|TestLooseRecord_HandOverFlushesBeforePostProcessing|TestLooseRecord_FinalizerFlushesBeforeTheJobLeaves)$

# Each mutation below takes away the event one loose-record wait is for, so
# the wait can only end at its deadline (lrWaitDeadline): KILLED shows that
# the deadline still fires and the test names the wait that failed, rather
# than hanging until the test binary's -timeout panics.

[a verification that succeeds never attaches the job]
file internal/app/residency.go
--- anchor
	if err := j.AttachFiles(m.Files()); err != nil {
		return err
	}
	if err := j.AttachArticles(m); err != nil {
		return err
	}
	// The message is a fact about the job, not the file: only the peek that
--- replace
	if m != nil {
		return nil
	}
	// The message is a fact about the job, not the file: only the peek that
--- end

[no paused job is verified after startup]
file internal/app/startup_reconcile.go
--- anchor
		if !ok || row.View.State != job.Fetching || j.Intent() != job.IntentPause {
--- replace
		if !ok || row.View.State != job.Fetching || j.Intent() != job.IntentPause || true {
--- end

[a write fault never stalls the job]
file internal/app/durability.go
--- anchor
		app.Stall(j, f)
--- replace
		_ = f
--- end

[the finalizer never reports the job's post-processing complete]
file internal/app/job_finalizer.go
--- anchor
		case app.postProcComplete <- PostProcComplete{JobID: ppJob.Job.ID()}:
--- replace
		case app.postProcComplete <- PostProcComplete{}:
--- end
