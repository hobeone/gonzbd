pkg ./internal/app/
run TestAddJob_DisconnectDuringDispatcherSaveStillPersistsQueueRow|TestRetryHistoryJob_DisconnectDuringDispatcherSaveStillPersistsQueueRow

[AddJob WithoutCancel neutered]
file internal/app/app.go
--- anchor
		addCtx, addCancel := context.WithTimeout(context.WithoutCancel(ctx), addPersistTimeout)
		err := app.dispatcher.Add(addCtx, j, hdr)
		addCancel()
		if err != nil {
			return fmt.Errorf("app: add to dispatcher: %w", err)
		}
--- replace
		addCtx, addCancel := context.WithTimeout(ctx, addPersistTimeout)
		err := app.dispatcher.Add(addCtx, j, hdr)
		addCancel()
		if err != nil {
			return fmt.Errorf("app: add to dispatcher: %w", err)
		}
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
