pkg ./internal/app/
run Test(RetryHistoryJob_AbortsWhenTheAssemblerCannotForgetTheJob|RetryHistoryJob_InProcessRetryDownloads)$

# A retry under the ID of a job that went through post-processing in this
# process downloads only because ForgetJob clears the assembler's tombstones,
# so a ForgetJob that fails aborts the retry.

[a failed ForgetJob is only logged]
file internal/app/app.go
--- anchor
			return fmt.Errorf("app: retry %s: clear the assembler's tombstones: %w", jobID, err)
--- replace
			_ = err
--- end

[ForgetJob leaves the job-level tombstone]
file internal/assembler/assembler.go
--- anchor
		delete(cancelledJobs, forgetID)
--- replace
		_ = forgetID
--- end
