pkg ./internal/app/
run TestPersistAndCommit_LeavesALaterInstanceAlone$

# persistAndCommit acting only on its own instance of a job, each half removed
# on its own. The yield's instance check (YieldedJob rather than Yielded by
# ID) has no mutation here: the later instance never holds a launch claim,
# which only a launched worker takes, so parking it by ID changes nothing the
# test can see. YieldedFor's own check is pinned in internal/dispatch by
# yielded_job_identity.spec.

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
