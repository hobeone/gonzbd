pkg ./internal/app/
run TestStopAndJoin_WaitsForWgBeforePostProcAndReportsTimeout

[stopAndJoin skips cancelling the application context]
file internal/app/export_test.go
--- anchor
	if a.cancel != nil {
		a.cancel()
	}
--- replace
	if false && a.cancel != nil {
		a.cancel()
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
