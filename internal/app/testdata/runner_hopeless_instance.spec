pkg ./internal/app/
run TestFailHopeless_LeavesALaterInstanceAlone$|TestMaybeReleaseRecoveryVolumes_LeavesALaterInstanceAlone$

# runAssess acting only on the instance it resolved, each instance-bound call
# reverted to its by-ID form on its own.

[the hopeless verdict hands over whatever instance holds the ID]
file internal/app/runner.go
--- anchor
	for _, reason := range reasons {
		r.app.maybeFinalizeJob(j, reason)
--- replace
	for _, reason := range reasons {
		r.app.maybeFinalize(j.ID(), reason)
--- end

[the hopeless verdict settles whatever instance holds the ID]
file internal/app/runner.go
--- anchor
		_ = r.report.FinishedJob(j, job.OutcomeFailed)
--- replace
		if cur, ok := r.app.dispatcher.Job(j.ID()); ok {
			_ = r.report.FinishedJob(cur, job.OutcomeFailed)
		}
--- end

[the hand-over reads the header of whatever instance holds the ID]
file internal/app/app.go
--- anchor
	row, ok := app.dispatcher.RowJob(j)
--- replace
	row, ok := app.dispatcher.Row(j.ID())
--- end

[the par2 verdict acts on whatever instance holds the ID]
file internal/app/app.go
--- anchor
	jobID := j.ID()

	if !j.HasDeferredPar2() {
--- replace
	if cur, ok := app.dispatcher.Job(j.ID()); ok {
		j = cur
	}
	jobID := j.ID()

	if !j.HasDeferredPar2() {
--- end
