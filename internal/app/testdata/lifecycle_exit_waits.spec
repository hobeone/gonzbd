pkg ./internal/app/
run Test(RunStallRecheck_ExitsOnContextCancel|WatchCompletions_DrainsPendingOnContextCancel)$

# These tests wait for a goroutine to return after its context is cancelled;
# a goroutine that does not return is caught at lrAwait's guard.

[runStallRecheck ignores its context]
file internal/app/durability.go
--- anchor
		case <-ctx.Done():
			return
		case <-stallTicker.C:
--- replace
		case <-stallTicker.C:
--- end

[watchCompletions keeps looping after it has drained on cancel]
file internal/app/app.go
--- anchor
			app.drainCompletions(ctx)
			return
--- replace
			app.drainCompletions(ctx)
--- end
