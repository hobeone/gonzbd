pkg ./internal/app/
run ^TestStall_LeavesALiveAssessingWorkerAlone$

# Stall releases only a Fetching worker. The first mutation is the shape it
# had: a yield by ID, which parks whatever worker the job has now.

[Stall yields by ID, whatever the state]
file internal/app/durability.go
--- anchor
		if j, ok := app.dispatcher.Job(jobID); ok {
			_ = app.dispatcher.YieldedFrom(j, job.Fetching)
		}
--- replace
		_ = app.dispatcher.Yielded(jobID)
--- end

[Stall yields from the state the job is at]
file internal/app/durability.go
--- anchor
			_ = app.dispatcher.YieldedFrom(j, job.Fetching)
--- replace
			_ = app.dispatcher.YieldedFrom(j, j.Snapshot().State.State)
--- end
