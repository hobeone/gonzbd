pkg ./internal/postproc/
run TestHasJob_AnotherInstanceUnderTheSameID$

# HasJob's instance comparison reverted to an ID comparison, the queued half
# and the in-flight half each on its own.

[a queued job answers for any instance under its ID]
file internal/postproc/has.go
--- anchor
			if queued.Job == j {
--- replace
			if queued.JobID() == j.ID() {
--- end

[the in-flight job answers for any instance under its ID]
file internal/postproc/has.go
--- anchor
		found = p.currentJob != nil && p.currentJob.Job == j
--- replace
		found = p.currentJob != nil && p.currentJob.JobID() == j.ID()
--- end
