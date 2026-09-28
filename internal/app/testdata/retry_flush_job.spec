pkg ./internal/app/
run TestRetryHistoryJob_AnotherJobsCheckpointFailureDoesNotFailTheRetry|TestRetryHistoryJob_ResumesCompletedFilesFromRetainedProgress
timeout 3m

[the retry flushes every dirty job, so another job's write failure fails it]
file internal/app/app.go
--- anchor
		if err := app.checkpointer.FlushJob(context.Background(), j); err != nil {
--- replace
		if err := app.checkpointer.Flush(context.Background()); err != nil {
--- end

[the retry does not flush its own job before admitting it]
file internal/app/app.go
--- anchor
		if err := app.checkpointer.FlushJob(context.Background(), j); err != nil {
--- replace
		if err := error(nil); err != nil {
--- end
