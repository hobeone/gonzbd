pkg ./internal/app/
run TestRemoveJob_DisconnectAfterDispatcherRemoveStillClearsDurability

# Three mutations for one fix, because the fix has three parts and each can be
# reverted alone: the detachment itself, and each of its two uses. A test that
# only died on the first would say nothing about whether both call sites are
# actually reading cleanupCtx.

[the detachment removed: cleanup runs on the caller's own context again]
file internal/app/app.go
--- anchor
	cleanupCtx := context.WithoutCancel(ctx)
--- replace
	cleanupCtx := ctx
--- end

[only the assembler half reverted: file handles never confirmed closed]
file internal/app/app.go
--- anchor
	cancelCtx, cancelCancel := context.WithTimeout(cleanupCtx, 30*time.Second)
--- replace
	cancelCtx, cancelCancel := context.WithTimeout(ctx, 30*time.Second)
--- end

[only the durability half reverted: the three deletes run on a done context]
file internal/app/app.go
--- anchor
	delCtx, delCancel := context.WithTimeout(cleanupCtx, 5*time.Second)
--- replace
	delCtx, delCancel := context.WithTimeout(ctx, 5*time.Second)
--- end
