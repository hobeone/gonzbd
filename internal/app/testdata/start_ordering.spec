pkg ./internal/app/
run TestStart_StartedNeverTrueBeforeCtxAssigned|TestStart_SecondCallLeaksNothing|TestStart_DoubleStartReturnsError|TestStart_FailedStartResetsStartedFlag
timeout 5m

[started flips true before app.ctx/app.cancel are assigned]
file internal/app/app.go
--- anchor
	app.ctx, app.cancel = context.WithCancel(ctx)
	// started flips true only here, after app.ctx/app.cancel are already
	// assigned above — see the ordering note in this method's doc comment.
	app.started.Store(true)
	if app.startedTransitionHook != nil {
		app.startedTransitionHook()
	}
--- replace
	// started flips true only here, after app.ctx/app.cancel are already
	// assigned above — see the ordering note in this method's doc comment.
	app.started.Store(true)
	if app.startedTransitionHook != nil {
		app.startedTransitionHook()
	}
	app.ctx, app.cancel = context.WithCancel(ctx)
--- end

[startedTransitionHook is never invoked]
file internal/app/app.go
--- anchor
	app.started.Store(true)
	if app.startedTransitionHook != nil {
		app.startedTransitionHook()
	}
--- replace
	app.started.Store(true)
--- end

[a second concurrent Start is no longer rejected before it can create a context]
file internal/app/app.go
--- anchor
	if !app.starting.CompareAndSwap(false, true) {
		return ErrAlreadyStarted
	}
--- replace
	if false {
		return ErrAlreadyStarted
	}
--- end

[a failed Start leaves starting permanently claimed, so no later Start can ever win the entry CAS again]
file internal/app/app.go
--- anchor
		app.started.Store(false)
		app.starting.Store(false)
	}()
--- replace
		app.started.Store(false)
	}()
--- end
