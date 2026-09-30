pkg ./internal/app/
run TestPersistAndCommit_RefusesARetryWhileItCommits$|TestRetryHistoryJob_RefusesWhenAFinalizerStartsDuringIt$|TestPruneHistory_SkipsAJobBeingFinalized$

# The finalizing record that keeps a retry off an ID while its finalizer
# commits, each part neutered on its own. RetryHistoryJob meets the record
# twice, at tryAcquire and before it registers, so a retry that starts during
# the commit is refused by both; the tryAcquire refusal is killed by the
# retention sweep, its other caller.

[the finalizer never records its ID]
file internal/app/job_finalizer.go
--- anchor
		defer app.transitions.beginFinalize(ppJob.Job.ID())()
--- replace
		defer func() {}()
--- end

[the finalizer never ends its record]
file internal/app/job_finalizer.go
--- anchor
		defer app.transitions.beginFinalize(ppJob.Job.ID())()
--- replace
		app.transitions.beginFinalize(ppJob.Job.ID())
--- end

[tryAcquire claims an ID a finalizer is committing]
file internal/app/transition.go
--- anchor
		if t.finalizing[id] > 0 {
			continue
		}
--- replace
		if false {
			continue
		}
--- end

[a retry that claimed the ID first registers under the finalizer]
file internal/app/app.go
--- anchor
	if app.transitions.isFinalizing(jobID) {
--- replace
	if false {
--- end
