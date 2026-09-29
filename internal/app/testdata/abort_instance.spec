pkg ./internal/app/
run TestAbort_YieldsAnInstancePostProcessingDoesNotHold$

# appWorkers.Abort asking the post-processor about its own instance rather than
# the job's ID. The abort has no by-ID question left to ask, so the mutation is
# made where the answer is computed: the in-flight comparison in HasJob, which
# is where the test's earlier instance sits. HasJob's queued half is pinned in
# internal/postproc by hasjob_instance.spec.

[post-processing running an earlier instance holds the later one's claim]
file internal/postproc/has.go
--- anchor
		found = p.currentJob != nil && p.currentJob.Job == j
--- replace
		found = p.currentJob != nil && p.currentJob.JobID() == j.ID()
--- end
