pkg ./internal/app/
run TestRetryHistoryJob_AnotherJobsCheckpointFailureDoesNotFailTheRetry|TestRetryHistoryJob_ResumesCompletedFilesFromRetainedProgress|TestRetryHistoryJob_FailedFlushLeavesNothingMarked
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

[an abandoned retry is reclaimed without being pruned, so its failed checkpoint stays marked]
file internal/app/app.go
--- anchor
		if app.checkpointer != nil {
			app.checkpointer.Prune(j)
		}
		delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
--- replace
		delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
--- end
