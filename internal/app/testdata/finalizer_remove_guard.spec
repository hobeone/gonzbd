pkg ./internal/app/
run TestFinalize_KeepsTheManifestWhenTheDispatcherRemoveFails

# The finalizer's copy of #376's ordering: a job whose queue row survived a
# failed Remove keeps its manifest and its rows. Two mutations, because two
# separate checks carry the two halves and reverting one says nothing about the
# other.

[reclaim no longer checks whether the dispatcher still holds the job]
file internal/app/durability.go
--- anchor
			if _, held := app.dispatcher.Job(jobID); held {
--- replace
			if _, held := app.dispatcher.Job(jobID); held && false {
--- end

[the reclaim rule no longer checks for a queue row]
file internal/durability/reclaim.go
--- anchor
 WHERE NOT EXISTS (SELECT 1 FROM dispatch_jobs d WHERE d.id = ` + t.name + `.job_id)`
--- replace
 WHERE 1 = 1`
--- end
