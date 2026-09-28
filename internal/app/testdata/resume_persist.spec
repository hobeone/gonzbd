pkg ./internal/app/
run TestResumeAllJobs_ClearedCompleteSurvivesRehydration
timeout 3m

[the sweep flushes without marking the job, so the cleared Complete stays in memory]
file internal/app/resume_startup.go
--- anchor
				app.checkpointer.Mark(j)
				if err := app.checkpointer.FlushJob(ctx, j); err != nil {
--- replace
				if err := app.checkpointer.FlushJob(ctx, j); err != nil {
--- end

[the sweep marks the job but does not flush it before eviction can re-hydrate]
file internal/app/resume_startup.go
--- anchor
				if err := app.checkpointer.FlushJob(ctx, j); err != nil {
--- replace
				if err := error(nil); err != nil {
--- end
