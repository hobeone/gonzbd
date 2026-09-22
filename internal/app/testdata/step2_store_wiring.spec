pkg ./internal/app/
run TestRestoreResolution_|TestCheckpointJob_LeavesThePendingBytesWhenTheRunFails|TestCheckpointJob_KeepsAJobAtRiskWhileItsBarrierIsInFlight|TestCheckpointJob_IsSerialisedPerJob|TestDropJobDurability_ReportsBothOwnersFailures|TestDeleteJobDurability_RemovesAllThreeTables|TestDeleteJobDurability_ReportsAFailedDelete|TestRetryHistoryJob_ClearsTheFailedArticlesItJustReset|TestRetryHistoryJob_AbortsWhenStaleRowsCannotBeDropped|TestAppCheckpointStore_SaveBatch|TestSeedJobFiles_|TestRecordAssembledCRC_
timeout 3m

[residency applies failed marks over an unreadable run record]
file internal/app/residency.go
--- anchor
		r.log.Warn("residency: read durable_runs", "job", j.ID(), "err", err)
		if !errors.Is(err, durability.ErrIncomplete) {
--- replace
		r.log.Warn("residency: read durable_runs", "job", j.ID(), "err", err)
		if false {
--- end

[the barrier ignores its CommitWrap]
file internal/durability/barrier.go
--- anchor
	if b.wrap == nil {
		return b.runs.commit(ctx, jobID, arts)
	}
--- replace
	if true {
		return b.runs.commit(ctx, jobID, arts)
	}
--- end

[dropJobDurability stops at the run store's failure]
file internal/app/durability.go
--- anchor
		errs = append(errs, fmt.Errorf("durable runs: %w", err))
--- replace
		return fmt.Errorf("durable runs: %w", err)
--- end

[deleteJobDurability leaves job_files behind]
file internal/app/durability.go
--- anchor
		if err := app.durable.DiscardFileRows(ctx, jobID); err != nil {
--- replace
		if err := error(nil); err != nil {
--- end

[dropJobDurability leaves failed_articles behind]
file internal/app/durability.go
--- anchor
	if err := app.durable.DiscardFailedArticles(ctx, jobID); err != nil {
		errs = append(errs, fmt.Errorf("failed articles: %w", err))
--- replace
	if err := error(nil); err != nil {
		errs = append(errs, fmt.Errorf("failed articles: %w", err))
--- end

[the retry keeps its stale failed marks]
file internal/app/app.go
--- anchor
		if err := app.durable.DiscardFailedArticles(ctx, jobID); err != nil {
			app.log.Warn("could not clear failed_articles for retry", "job", jobID, "err", err)
--- replace
		if err := error(nil); err != nil {
			app.log.Warn("could not clear failed_articles for retry", "job", jobID, "err", err)
--- end

[the checkpoint adapter drops failed marks]
file internal/app/dispatcher_wiring.go
--- anchor
			if p.ArticlesFailed() > 0 {
--- replace
			if false {
--- end
