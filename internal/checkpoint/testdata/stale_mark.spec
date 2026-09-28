pkg ./internal/checkpoint/
run TestMark_|TestUnprune_|TestPrune_ForgetsACollectedInstance
timeout 2m

[Mark does not consult the refusal, so a pruned instance's late mark is kept]
file internal/checkpoint/checkpointer.go
--- anchor
	if _, gone := c.pruned[key]; gone {
--- replace
	if _, gone := c.pruned[key]; gone && false {
--- end

[Prune does not record the instance, so nothing refuses its later marks]
file internal/checkpoint/checkpointer.go
--- anchor
	if _, ok := c.pruned[key]; !ok {
		c.pruned[key] = runtime.AddCleanup(j, c.forgetPruned, key)
	}
--- replace
	if _, ok := c.pruned[key]; !ok && false {
		c.pruned[key] = runtime.AddCleanup(j, c.forgetPruned, key)
	}
--- end

[Unprune leaves the refusal in place]
file internal/checkpoint/checkpointer.go
--- anchor
		cleanup.Stop()
		delete(c.pruned, key)
--- replace
		cleanup.Stop()
--- end

[a collected instance's refusal is never forgotten, so the record grows with every prune]
file internal/checkpoint/checkpointer.go
--- anchor
		c.pruned[key] = runtime.AddCleanup(j, c.forgetPruned, key)
--- replace
		c.pruned[key] = runtime.Cleanup{}
--- end
