pkg ./internal/app/
run TestDropJobAlreadyInHistory_KeepsEverythingWhenTheDispatcherRemoveFails

# A duplicate whose removal fails must still be cancelled, which Remove does
# before the step that fails, so the tick does not route it onward. What it
# keeps is reclaim's to decide, and step3_call_sites.spec pins that.

[the duplicate is never handed to Dispatcher.Remove, so it stays runnable]
file internal/app/durability.go
--- anchor
		rmErr := app.dispatcher.Remove(rmCtx, jobID)
--- replace
		var rmErr error
		_ = rmCtx
--- end
