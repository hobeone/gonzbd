pkg ./internal/app/
run TestStopAndJoin_WaitsForWgBeforePostProcAndReportsTimeout

[joinAndStop skips cancelling the application context]
file internal/app/app.go
--- anchor
	if app.cancel != nil {
		app.cancel()
	}
--- replace
	if false && app.cancel != nil {
		app.cancel()
	}
--- end

[joinAndStop stops the post-processor before waiting on wg]
file internal/app/app.go
--- anchor
	if err := waitBounded("wg.Wait", stepTimeout, func() error {
		app.wg.Wait()
		return nil
	}, app.log); err != nil {
		*errs = append(*errs, fmt.Errorf("wg wait: %w", err))
	}

	ppStopFn := app.postProcessor.Stop
	if app.postProcStopHook != nil {
		ppStopFn = app.postProcStopHook
	}
	ppErr := waitBounded("postprocessor", stepTimeout, ppStopFn, app.log)
	if ppErr != nil {
		*errs = append(*errs, fmt.Errorf("postprocessor stop: %w", ppErr))
	}
--- replace
	ppStopFn := app.postProcessor.Stop
	if app.postProcStopHook != nil {
		ppStopFn = app.postProcStopHook
	}
	ppErr := waitBounded("postprocessor", stepTimeout, ppStopFn, app.log)
	if ppErr != nil {
		*errs = append(*errs, fmt.Errorf("postprocessor stop: %w", ppErr))
	}

	if err := waitBounded("wg.Wait", stepTimeout, func() error {
		app.wg.Wait()
		return nil
	}, app.log); err != nil {
		*errs = append(*errs, fmt.Errorf("wg wait: %w", err))
	}
--- end

[stopAndJoin swallows waitBounded errors instead of returning them]
file internal/app/export_test.go
--- anchor
	return errors.Join(errs...)
--- replace
	_ = errors.Join(errs...)
	return nil
--- end
