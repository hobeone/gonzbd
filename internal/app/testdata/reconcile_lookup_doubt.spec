pkg ./internal/app/
run ^(TestDropJobAlreadyInHistory_KeepsTheJobWhenTheHistoryLookupFails|TestHoldUnreconciledJob_LeavesACancelledJobAlone)$

# Doubt must neither remove the job nor leave it runnable. Each mutation
# mutates a branch or a call rather than the errors.Is, because neutering the
# condition leaves the history import unused and a compile error says nothing
# about whether the test would have caught the behaviour.

[doubt is treated as "in history", so a job that may not be filed is removed]
file internal/app/durability.go
--- anchor
			app.holdUnreconciledJob(jobID, err)
		}
		return
	}
--- replace
			entry = &history.Entry{}
		} else {
			return
		}
	}
--- end

[doubt leaves the job runnable, so the first tick post-processes it]
file internal/app/durability.go
--- anchor
			app.holdUnreconciledJob(jobID, err)
--- replace
			_ = err
--- end

[the hold logs but never pauses the job]
file internal/app/durability.go
--- anchor
	if err := app.dispatcher.PauseJob(jobID); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[the hold pauses the job without telling the operator why]
file internal/app/durability.go
--- anchor
	_ = app.dispatcher.SetOperationalError(jobID, "history lookup failed at startup: "+
--- replace
	_ = app.dispatcher.SetOperationalError(jobID, ""+
--- end

[a hold with no dispatcher reaches for one]
file internal/app/durability.go
--- anchor
	if app.dispatcher == nil {
		return
	}
	if err := app.dispatcher.PauseJob(jobID); err != nil {
--- replace
	if false {
		return
	}
	if err := app.dispatcher.PauseJob(jobID); err != nil {
--- end

[a refused pause still notes the job as held]
file internal/app/durability.go
--- anchor
			"job", jobID, "err", err)
		return
	}
	_ = app.dispatcher.SetOperationalError(jobID, "history lookup failed at startup: "+
--- replace
			"job", jobID, "err", err)
	}
	_ = app.dispatcher.SetOperationalError(jobID, "history lookup failed at startup: "+
--- end
