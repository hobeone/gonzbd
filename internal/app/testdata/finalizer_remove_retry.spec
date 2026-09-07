pkg ./internal/app/
run TestFinalizer_RemoveError_RetriesAndSucceeds

[finalizer dispatcher remove retry neutered]
file internal/app/job_finalizer.go
--- anchor
			if err != nil && occupyCtx.Err() == nil {
--- replace
			if false {
--- end
