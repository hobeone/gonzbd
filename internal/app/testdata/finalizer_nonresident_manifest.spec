pkg ./internal/app/
run TestFinalizer_FailedJob_NonResidentManifest_WritesHistoryJobFiles

[non-resident manifest fallback in persistAndCommit neutered]
file internal/app/job_finalizer.go
--- anchor
				if mErr != nil && errors.Is(mErr, job.ErrNotResident) {
--- replace
				if false && errors.Is(mErr, job.ErrNotResident) {
--- end
