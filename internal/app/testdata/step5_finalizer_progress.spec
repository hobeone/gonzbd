pkg ./internal/app/
run TestFinalizer_FailedJob_NonResidentManifest_WritesHistoryJobFiles

[the finalizer stops gathering a failed job's retained progress]
file internal/app/job_finalizer.go
--- anchor
			if entry.Status == string(constants.StatusFailed) && ppJob != nil && ppJob.Job != nil {
--- replace
			if entry.Status != string(constants.StatusFailed) && ppJob != nil && ppJob.Job != nil {
--- end
