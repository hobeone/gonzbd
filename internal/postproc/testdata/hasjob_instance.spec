pkg ./internal/postproc/
run TestHasJob_AnotherInstanceUnderTheSameID$

# HasJob's instance comparison reverted to an ID comparison, the queued half
# and the in-flight half each on its own.

[a queued job answers for any instance under its ID]
file internal/postproc/has.go
--- anchor
		found = slices.ContainsFunc(jobs, func(queued *Job) bool { return queued.Job == j })
--- replace
		found = slices.ContainsFunc(jobs, func(queued *Job) bool { return queued.JobID() == j.ID() })
--- end

[the in-flight job answers for any instance under its ID]
file internal/postproc/has.go
--- anchor
		found = p.currentJob != nil && p.currentJob.Job == j
--- replace
		found = p.currentJob != nil && p.currentJob.JobID() == j.ID()
--- end
