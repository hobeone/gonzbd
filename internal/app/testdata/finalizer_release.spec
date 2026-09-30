pkg ./internal/app/
run TestFinalizer_PersistError_ReleasesDispatcherResources

[early dispatcher release dropped from persistAndCommit]
file internal/app/job_finalizer.go
--- anchor
	if app.dispatcher != nil {
		id := ppJob.Job.ID()
		warnUnlessGone(log, "finalize: cancelling the job failed", id,
			app.dispatcher.CancelJob(ppJob.Job))
		warnUnlessGone(log, "finalize: releasing the job's launch claim failed", id,
			app.dispatcher.YieldedJob(ppJob.Job))
	}
--- replace
	if false {
	}
--- end
