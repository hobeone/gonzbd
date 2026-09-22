pkg ./internal/app/
run TestAddJob_DisconnectDuringDispatcherSaveStillPersistsQueueRow|TestRetryHistoryJob_DisconnectDuringDispatcherSaveStillPersistsQueueRow|TestAddJob_FailedAddLeavesNoOrphanArtifacts|TestAddJob_FailedSeedLeavesNoOrphanArtifacts|TestRetryHistoryJob_FailedAddRemovesTheQueueManifest|TestDiscardUnaddedJobArtifacts_SurvivesRemovalFailures|TestRetryHistoryJob_FailedFlushRemovesTheQueueManifest

[AddJob WithoutCancel neutered]
file internal/app/app.go
--- anchor
		addCtx, addCancel := context.WithTimeout(context.WithoutCancel(ctx), addPersistTimeout)
		err := app.dispatcher.Add(addCtx, j, hdr)
		addCancel()
		if err != nil {
			return fmt.Errorf("app: add to dispatcher: %w", err)
--- replace
		addCtx, addCancel := context.WithTimeout(ctx, addPersistTimeout)
		err := app.dispatcher.Add(addCtx, j, hdr)
		addCancel()
		if err != nil {
			return fmt.Errorf("app: add to dispatcher: %w", err)
--- end

[RetryHistoryJob WithoutCancel neutered]
file internal/app/app.go
--- anchor
		addCtx, addCancel := context.WithTimeout(context.WithoutCancel(ctx), addPersistTimeout)
		err := app.dispatcher.Add(addCtx, j, hdr)
		addCancel()
		if err != nil {
			return err
		}
--- replace
		addCtx, addCancel := context.WithTimeout(ctx, addPersistTimeout)
		err := app.dispatcher.Add(addCtx, j, hdr)
		addCancel()
		if err != nil {
			return err
		}
--- end

[AddJob's deferred cleanup neutered]
file internal/app/app.go
--- anchor
		if !admitted {
			app.discardUnaddedJobArtifacts(ctx, j.ID(), hdr.NZBBackup)
		}
--- replace
		if !admitted {
			_ = ctx
		}
--- end

[AddJob's cleanup narrowed back to the dispatcher's failure alone]
file internal/app/app.go
--- anchor
	// it, not just the Add that happens to be last.
	admitted := false
--- replace
	// it, not just the Add that happens to be last.
	admitted := true
--- end

[RetryHistoryJob's deferred manifest cleanup neutered]
file internal/app/app.go
--- anchor
			app.reclaim(delCtx, jobID)
		}()
--- replace
			_ = delCtx
		}()
--- end

[RetryHistoryJob's cleanup narrowed back to the dispatcher's failure alone]
file internal/app/app.go
--- anchor
	}
	admitted := false
--- replace
	}
	admitted := true
--- end

[RetryHistoryJob history delete re-attached to the caller's context]
file internal/app/app.go
--- anchor
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), addPersistTimeout)
--- replace
	delCtx, delCancel := context.WithTimeout(ctx, addPersistTimeout)
--- end
