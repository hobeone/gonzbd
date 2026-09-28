pkg ./internal/app/
run ^(TestCheckpoint_StaleMarkAfterPruneDoesNotReachARetry|TestRemoveJob_FailedRemovalKeepsCheckpointingTheJob)$
timeout 3m

[Mark does not consult the refusal, so a departed instance's late failed articles reach the retry]
file internal/checkpoint/checkpointer.go
--- anchor
	if _, gone := c.pruned[key]; gone {
--- replace
	if _, gone := c.pruned[key]; gone && false {
--- end

[a RemoveJob whose dispatcher.Remove failed keeps refusing the still-registered job's marks]
file internal/app/app.go
--- anchor
			app.checkpointer.Unprune(j)
--- replace
			_ = j
--- end
