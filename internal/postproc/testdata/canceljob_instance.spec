pkg ./internal/postproc/
run TestCancelJob_AnotherInstanceUnderTheSameID$

# CancelJob's instance comparison reverted to an ID comparison, the queued half
# and the in-flight half each on its own.

[a queued job is cancelled for any instance under its ID]
file internal/postproc/cancel.go
--- anchor
	if idx := slices.IndexFunc(q.jobs, func(queued *Job) bool { return queued.Job == j }); idx >= 0 {
--- replace
	if idx := slices.IndexFunc(q.jobs, func(queued *Job) bool { return queued.JobID() == j.ID() }); idx >= 0 {
--- end

[the in-flight job is interrupted for any instance under its ID]
file internal/postproc/cancel.go
--- anchor
	inFlight := p.currentJob != nil && p.currentJob.Job == j && p.currentJobCancel != nil
--- replace
	inFlight := p.currentJob != nil && p.currentJob.JobID() == j.ID() && p.currentJobCancel != nil
--- end
