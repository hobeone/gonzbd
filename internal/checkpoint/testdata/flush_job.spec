pkg ./internal/checkpoint/
run TestFlushJob_|TestPrune_WaitsForAFlushAcrossAConcurrentFlushJob|TestPrune_WaitsForAFlushJobCarryingTheJob
timeout 3m

[FlushJob takes the whole dirty set, so it writes and fails on every other job]
file internal/checkpoint/checkpointer.go
--- anchor
		delete(c.dirty, id)
		return map[string]*job.Job{id: j}
--- replace
		batch := c.dirty
		c.dirty = make(map[string]*job.Job)
		return batch
--- end

[FlushJob writes j whether or not the dirty set holds it]
file internal/checkpoint/checkpointer.go
--- anchor
		if c.dirty[id] != j {
--- replace
		if false {
--- end

[FlushJob keys on the ID, not the instance]
file internal/checkpoint/checkpointer.go
--- anchor
		if c.dirty[id] != j {
--- replace
		if c.dirty[id] == nil {
--- end

[FlushJob leaves the job it wrote in the dirty set]
file internal/checkpoint/checkpointer.go
--- anchor
		delete(c.dirty, id)
		return map[string]*job.Job{id: j}
--- replace
		return map[string]*job.Job{id: j}
--- end

[FlushJob does not share flushMu with Flush]
file internal/checkpoint/checkpointer.go
--- anchor
	id := j.ID()
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
--- replace
	id := j.ID()
--- end
