pkg ./internal/app/
run TestReclaim_|TestRemoveJob_Reclaims|TestRemoveJob_DisconnectAfterDispatcherRemoveStillClearsDurability|TestPersistAndCommit_|TestFinalize_|TestDropJobAlreadyInHistory_AppliesTheFailedRetentionRule|TestRemoveHistoryJob_ReclaimsTheFailedEntrysRuns|TestMarkHistoryCompleted_|TestAddJob_FailedAddLeavesNoOrphanArtifacts|TestRetryHistoryJob_|TestStart_SweepsWhatNoDepartureReclaimed|TestSweepOrphans_
timeout 5m

[RemoveJob skips the reclaim when someone else removed the job]
file internal/app/app.go
--- anchor
		app.reclaim(delCtx, id)
		delCancel()
		app.emit(Event{Type: "queue_updated"})
		return nil
--- replace
		_ = delCtx
		delCancel()
		app.emit(Event{Type: "queue_updated"})
		return nil
--- end

[RemoveJob skips the reclaim after its own Remove]
file internal/app/app.go
--- anchor
	delCtx, delCancel := context.WithTimeout(cleanupCtx, 5*time.Second)
	app.reclaim(delCtx, id)
--- replace
	delCtx, delCancel := context.WithTimeout(cleanupCtx, 5*time.Second)
	_ = delCtx
--- end

[finalize skips the reclaim]
file internal/app/job_finalizer.go
--- anchor
			app.reclaim(delCtx, ppJob.Job.ID())
--- replace
			_ = delCtx
--- end

[startup reconciliation skips the reclaim]
file internal/app/durability.go
--- anchor
	app.reclaim(delCtx, jobID)
	delCancel()
	return true
--- replace
	_ = delCtx
	delCancel()
	return true
--- end

[a history delete skips the reclaim]
file internal/app/app.go
--- anchor
	app.reclaim(delCtx, ids[0], ids[1:]...)
--- replace
	_ = delCtx
--- end

[mark-completed skips the reclaim]
file internal/app/app.go
--- anchor
	if err := app.historyRepo.MarkCompleted(ctx, id); err != nil {
		return err
	}
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer delCancel()
	app.reclaim(delCtx, id)
--- replace
	if err := app.historyRepo.MarkCompleted(ctx, id); err != nil {
		return err
	}
--- end

[a failed AddJob skips the reclaim]
file internal/app/app.go
--- anchor
	defer delCancel()
	app.reclaim(delCtx, jobID)
}
--- replace
	defer delCancel()
	_ = delCtx
}
--- end

[a retry that never entered the queue skips the reclaim]
file internal/app/app.go
--- anchor
			defer delCancel()
			app.reclaim(delCtx, jobID)
		}()
--- replace
			defer delCancel()
			_ = delCtx
		}()
--- end

[the retry leaves stray failed marks]
file internal/app/app.go
--- anchor
		if err := app.durable.Reclaim(ctx, jobID); err != nil {
			app.log.Warn("could not clear failed_articles for retry", "job", jobID, "err", err)
--- replace
		if err := error(nil); err != nil {
			app.log.Warn("could not clear failed_articles for retry", "job", jobID, "err", err)
--- end

[Start skips the sweep]
file internal/app/app.go
--- anchor
	app.sweepOrphans(app.ctx)
--- replace
--- end

[the sweep leaves stranded manifests]
file internal/app/durability.go
--- anchor
	if len(ids) > 0 {
		app.reclaim(ctx, ids[0], ids[1:]...)
	}
--- replace
	_ = ids
--- end

[reclaim unlinks the manifest of a job the dispatcher still holds]
file internal/app/durability.go
--- anchor
			if _, held := app.dispatcher.Job(jobID); held {
--- replace
			if _, held := app.dispatcher.Job(jobID); held && false {
--- end

[reclaim swallows a failed reclaim]
file internal/app/durability.go
--- anchor
			app.log.Warn("could not reclaim a departed job's rows; the next startup's sweep will",
				"job", id, "more", len(more), "err", err)
--- replace
			_ = err
--- end

[reclaim leaves manifests when the rows could not be reclaimed]
file internal/app/durability.go
--- anchor
	dir := manifestDir(app.config.GetGeneral().AdminDir)
	for _, jobID := range append([]string{id}, more...) {
--- replace
	dir := manifestDir(app.config.GetGeneral().AdminDir)
	for _, jobID := range append([]string{id}, more...)[:0] {
--- end

[the retry ignores its retained file progress]
file internal/app/app.go
--- anchor
		for _, f := range retained {
			_ = j.RestoreFileMeta(f.FileIndex, f.Filename, f.Complete, f.AssembledCRC32)
		}
--- replace
		for _, f := range retained[:0] {
			_ = j.RestoreFileMeta(f.FileIndex, f.Filename, f.Complete, f.AssembledCRC32)
		}
--- end

[the retry never persists the progress it restored]
file internal/app/app.go
--- anchor
		if err := app.checkpointer.Flush(context.Background()); err != nil {
			return fmt.Errorf("app: retry %s: flush checkpoint: %w", jobID, err)
--- replace
		if err := error(nil); err != nil {
			return fmt.Errorf("app: retry %s: flush checkpoint: %w", jobID, err)
--- end

[a failed rule statement no longer rolls back the ones before it]
file internal/durability/reclaim.go
--- anchor
	if err := fn(tx); err != nil {
		return err
	}
--- replace
	_ = fn(tx)
--- end

[the sweep swallows a failed row sweep]
file internal/app/durability.go
--- anchor
			app.log.Warn("startup sweep of unreachable durability rows failed", "err", err)
--- replace
			_ = err
--- end

[the sweep swallows a failed manifest listing]
file internal/app/durability.go
--- anchor
			app.log.Warn("startup sweep could not list the manifests", "err", err)
--- replace
			_ = err
--- end

[the sweep reports a missing manifests directory as a failure]
file internal/app/durability.go
--- anchor
		if !os.IsNotExist(err) {
			app.log.Warn("startup sweep could not list the manifests", "err", err)
--- replace
		if true {
			app.log.Warn("startup sweep could not list the manifests", "err", err)
--- end

[RemoveJob skips the reclaim for a job that had already left the queue]
file internal/app/app.go
--- anchor
		delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		app.reclaim(delCtx, id)
		delCancel()
		return fmt.Errorf("job %q not found", id)
--- replace
		return fmt.Errorf("job %q not found", id)
--- end
