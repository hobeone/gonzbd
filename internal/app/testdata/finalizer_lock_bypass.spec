pkg ./internal/app/
run TestPersistAndCommit_(LeavesALaterInstanceAlone|WithoutTheLock_LeavesARetryRegisteredDuringTheTeardown)$

# persistAndCommit's teardown acting only on its own instance when it runs
# without the job's transition lock, each instance-bound call reverted to its
# by-ID form on its own.
#
# The removal's retry (the second RemoveJob) has no mutation here: it runs only
# after a first attempt failed with the instance registered, and no seam can
# deregister it and register a later instance between the two attempts.
# RemoveJob's own instance check is pinned in internal/dispatch by
# instance_doors.spec.

[the teardown removes whatever instance holds the ID]
file internal/app/job_finalizer.go
--- anchor
			err := app.dispatcher.RemoveJob(removeCtx, ppJob.Job)
--- replace
			err := app.dispatcher.Remove(removeCtx, jobID)
--- end

[a removal that found no instance is reported on whatever instance holds the ID]
file internal/app/job_finalizer.go
--- anchor
			if err != nil && !errors.Is(err, dispatch.ErrNotFound) {
--- replace
			if err != nil {
--- end

[the commit occupies whatever instance holds the ID]
file internal/app/job_finalizer.go
--- anchor
		if err := app.dispatcher.OccupyJob(finalCtx, ppJob.Job, func(occupyCtx context.Context) {
--- replace
		if err := app.dispatcher.Occupy(finalCtx, ppJob.Job.ID(), func(occupyCtx context.Context) {
--- end
