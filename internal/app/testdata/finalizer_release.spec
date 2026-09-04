pkg ./internal/app/
run TestFinalizer_PersistError_ReleasesDispatcherResources

[early dispatcher release dropped from persistAndCommit]
file internal/app/job_finalizer.go
--- anchor
	if app.dispatcher != nil && job != nil && job.Job != nil {
		_ = app.dispatcher.Cancel(job.Job.ID())
		_ = app.dispatcher.Yielded(job.Job.ID())
	}
--- replace
	if false {
	}
--- end
