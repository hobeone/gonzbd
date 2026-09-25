pkg ./internal/app/
run .
timeout 10m

[RemoveJob does not prune the checkpointer]
file internal/app/app.go
--- anchor
	if app.checkpointer != nil {
		app.checkpointer.Prune(id)
	}
--- replace
	if app.checkpointer != nil {
		_ = id
	}
--- end

[finalize does not prune the checkpointer]
file internal/app/job_finalizer.go
--- anchor
			app.checkpointer.Prune(ppJob.Job.ID())
--- replace
--- end
