pkg ./internal/app/
run TestResumeAtStartup_RestoredVerdictDoesNotOutrunTheSweep
timeout 5m

[the resume sweep runs after the dispatcher's ticker has started, as it did before]
file internal/app/app.go
--- anchor
		if err := app.dispatcher.StartWith(app.ctx, func(ctx context.Context) error {
			if err := app.resumeAllJobs(ctx); err != nil {
				return err
			}
			handOffs = app.startupHandOffs()
			return nil
		}); err != nil {
			return fmt.Errorf("app: start dispatcher: %w", err)
		}
	}
	if err := app.assembler.Start(app.ctx); err != nil {
		return err
	}
--- replace
		if err := app.dispatcher.Start(app.ctx); err != nil {
			return fmt.Errorf("app: start dispatcher: %w", err)
		}
	}
	if err := app.assembler.Start(app.ctx); err != nil {
		return err
	}
	if err := app.resumeAllJobs(app.ctx); err != nil {
		_ = app.assembler.Stop()
		return err
	}
	handOffs = app.startupHandOffs()
--- end
