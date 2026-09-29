pkg ./internal/dispatch/
run TestStartWith_RunsBeforeTheFirstTick|TestStartWith_StepErrorFailsStart

[beforeFirstTick is never called]
file internal/dispatch/dispatch.go
--- anchor
	if err == nil && beforeFirstTick != nil {
		err = beforeFirstTick(ctx)
	}
--- replace
	if false {
		err = beforeFirstTick(ctx)
	}
--- end

[a tick runs while beforeFirstTick does]
file internal/dispatch/dispatch.go
--- anchor
	if err == nil && beforeFirstTick != nil {
		err = beforeFirstTick(ctx)
	}
--- replace
	if err == nil && beforeFirstTick != nil {
		go d.tick(ctx)
		err = beforeFirstTick(ctx)
	}
--- end

[beforeFirstTick's error is dropped and Start proceeds]
file internal/dispatch/dispatch.go
--- anchor
	if err == nil && beforeFirstTick != nil {
		err = beforeFirstTick(ctx)
	}
--- replace
	if err == nil && beforeFirstTick != nil {
		_ = beforeFirstTick(ctx)
	}
--- end
