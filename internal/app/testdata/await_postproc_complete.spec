pkg ./internal/app/
run TestEnqueuePostProc_DeliveredCrashRecovery

[the dispatcher removal dropped from persistAndCommit]
file internal/app/job_finalizer.go
--- anchor
		err := app.dispatcher.RemoveJob(removeCtx, ppJob.Job)
--- replace
		var err error
		_ = removeCtx
--- end

[the post-processor left busy after the job is recorded]
file internal/postproc/postproc.go
--- anchor
		p.addHistory(job)
		p.setBusyWithJob(false, nil, nil)
--- replace
		p.addHistory(job)
--- end
