pkg ./internal/app/
run TestRestoreResolution_|TestCheckpointJob_LeavesThePendingBytesWhenTheRunFails|TestCheckpointJob_KeepsAJobAtRiskWhileItsBarrierIsInFlight|TestCheckpointJob_IsSerialisedPerJob|TestRetryHistoryJob_AbortsWhenStaleRowsCannotBeDropped|TestAppCheckpointStore_SaveBatch|TestSeedJobFiles_|TestRecordAssembledCRC_
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

[the checkpoint adapter drops failed marks]
file internal/app/dispatcher_wiring.go
--- anchor
			if p.ArticlesFailed() > 0 {
--- replace
			if false {
--- end
