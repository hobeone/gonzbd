pkg ./internal/app/
run TestPersistAndCommit_(LeavesALaterInstanceAlone|LeavesALaterInstancesLaunchClaimAlone)$

# persistAndCommit acting only on its own instance of a job, each half removed
# on its own.

[a later instance holding the ID does not stop the finalizer]
file internal/app/job_finalizer.go
--- anchor
			if cur, ok := app.dispatcher.Job(ppJob.Job.ID()); ok && cur != ppJob.Job {
--- replace
			if cur, ok := app.dispatcher.Job(ppJob.Job.ID()); ok && cur != ppJob.Job && false {
--- end

[the finalizer cancels whatever instance holds the ID]
file internal/app/job_finalizer.go
--- anchor
			app.dispatcher.CancelJob(ppJob.Job))
		warnUnlessGone(log, "finalize: releasing
--- replace
			app.dispatcher.Cancel(id))
		warnUnlessGone(log, "finalize: releasing
--- end

[the finalizer yields by ID instead of by instance]
file internal/app/job_finalizer.go
--- anchor
		warnUnlessGone(log, "finalize: releasing the job's launch claim failed", id,
			app.dispatcher.YieldedJob(ppJob.Job))
--- replace
		warnUnlessGone(log, "finalize: releasing the job's launch claim failed", id,
			app.dispatcher.Yielded(id))
--- end
