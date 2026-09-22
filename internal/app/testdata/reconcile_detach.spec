pkg ./internal/app/
run TestDropJobAlreadyInHistory_CancellationAfterRemoveStillClearsDurability

# The second call site of RemoveJob's fix, reverted on its own.
# internal/app/testdata/remove_job_detach.spec covers the first.

[the detachment removed: the reconcile's cleanup runs on the startup context]
file internal/app/durability.go
--- anchor
	delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	app.reclaim(delCtx, jobID)
--- replace
	delCtx, delCancel := context.WithTimeout(ctx, 5*time.Second)
	app.reclaim(delCtx, jobID)
--- end
