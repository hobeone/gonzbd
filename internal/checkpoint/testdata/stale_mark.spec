pkg ./internal/checkpoint/
run TestMark_|TestUnprune_|TestPrune_ForgetsACollectedInstance|TestPrune_LeavesALater|TestForgetPruned_
timeout 2m

[Mark does not consult the refusal, so a pruned instance's late mark is kept]
file internal/checkpoint/checkpointer.go
--- anchor
	if _, gone := c.pruned[key]; gone {
--- replace
	if _, gone := c.pruned[key]; gone && false {
--- end

[Prune records nothing, so nothing refuses its later marks]
file internal/checkpoint/checkpointer.go
--- anchor
		c.pruned[key] = rec
--- replace
		_ = rec
--- end

[Unprune leaves the refusal in place]
file internal/checkpoint/checkpointer.go
--- anchor
		rec.cleanup.Stop()
		delete(c.pruned, key)
--- replace
		rec.cleanup.Stop()
--- end

[one Unprune withdraws every Prune of the instance]
file internal/checkpoint/checkpointer.go
--- anchor
	if rec.prunes <= 0 {
--- replace
	if true {
--- end

[a second Prune of one instance is not counted]
file internal/checkpoint/checkpointer.go
--- anchor
	rec.prunes++
--- replace
	rec.prunes = 1
--- end

[a collected instance's refusal is never forgotten, so the record grows with every prune]
file internal/checkpoint/checkpointer.go
--- anchor
		rec = &pruneRecord{cleanup: runtime.AddCleanup(j, c.forgetPruned, key)}
--- replace
		rec = &pruneRecord{}
--- end

[Prune drops a later instance's pending mark under the same ID]
file internal/checkpoint/checkpointer.go
--- anchor
	if c.dirty[id] == j {
--- replace
	if true {
--- end

[Prune takes a later instance under the same ID out of a failing flush's re-merge]
file internal/checkpoint/checkpointer.go
--- anchor
	if c.inFlight[id] == j {
--- replace
	if true {
--- end
