pkg ./internal/app/
run Test(RetryHistoryJob_AbortsWhenTheAssemblerCannotForgetTheJob|RetryHistoryJob_InProcessRetryDownloads)$

# A retry under the ID of a job that went through post-processing in this
# process needs the assembler's per-file tombstones cleared, so a ForgetJob
# that fails aborts the retry.

[a failed ForgetJob is only logged]
file internal/app/app.go
--- anchor
			return fmt.Errorf("app: retry %s: clear the assembler's tombstones: %w", jobID, err)
--- replace
			_ = err
--- end
