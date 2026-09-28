pkg ./internal/app/
run ^(TestRemoveJob_WaitsForACheckpointThatIsWritingTheJob|TestFinalizer_PersistError_CleanupExecutes)$
timeout 2m

[RemoveJob does not prune the checkpointer]
file internal/app/app.go
--- anchor
	if app.checkpointer != nil {
		app.checkpointer.Prune(j)
	}
--- replace
	if app.checkpointer != nil {
		_ = j
	}
--- end

[finalize does not prune the checkpointer]
file internal/app/job_finalizer.go
--- anchor
			app.checkpointer.Prune(ppJob.Job)
--- replace
--- end
