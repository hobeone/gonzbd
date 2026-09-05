pkg ./internal/app/
run TestFinalizer_PersistError_ReleasesDispatcherResources

[early dispatcher release dropped from persistAndCommit]
file internal/app/job_finalizer.go
--- anchor
	if app.dispatcher != nil && ppJob != nil && ppJob.Job != nil {
		_ = app.dispatcher.Cancel(ppJob.Job.ID())
		_ = app.dispatcher.Yielded(ppJob.Job.ID())
	}
--- replace
	if false {
	}
--- end
