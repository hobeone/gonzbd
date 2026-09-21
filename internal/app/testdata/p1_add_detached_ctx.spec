pkg ./internal/app/
run TestAddJob_DisconnectDuringDispatcherSaveStillPersistsQueueRow|TestRetryHistoryJob_DisconnectDuringDispatcherSaveStillPersistsQueueRow|TestAddJob_FailedAddLeavesNoOrphanArtifacts

[AddJob WithoutCancel neutered]
file internal/app/app.go
--- anchor
		addCtx, addCancel := context.WithTimeout(context.WithoutCancel(ctx), addPersistTimeout)
		err := app.dispatcher.Add(addCtx, j, hdr)
		addCancel()
		if err != nil {
			// Everything above this line is already on disk: the NZB backup,
--- replace
		addCtx, addCancel := context.WithTimeout(ctx, addPersistTimeout)
		err := app.dispatcher.Add(addCtx, j, hdr)
		addCancel()
		if err != nil {
			// Everything above this line is already on disk: the NZB backup,
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

[AddJob orphan cleanup neutered]
file internal/app/app.go
--- anchor
			app.discardUnaddedJobArtifacts(ctx, j.ID(), hdr.NZBBackup)
--- replace
			// cleanup neutered
--- end

[RetryHistoryJob history delete re-attached to the caller's context]
file internal/app/app.go
--- anchor
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), addPersistTimeout)
--- replace
	delCtx, delCancel := context.WithTimeout(ctx, addPersistTimeout)
--- end
