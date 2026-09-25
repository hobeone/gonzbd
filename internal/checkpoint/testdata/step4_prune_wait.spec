pkg ./internal/checkpoint/
run TestPrune_|TestCheckpointer_
timeout 3m

[Prune does not wait for the flush carrying the job]
file internal/checkpoint/checkpointer.go
--- anchor
	if _, carried := c.flushing[id]; carried {
		done = c.flushDone
	}
--- replace
	if _, carried := c.flushing[id]; carried && false {
		done = c.flushDone
	}
--- end

[Prune waits for every flush, not only the one carrying the job]
file internal/checkpoint/checkpointer.go
--- anchor
	if _, carried := c.flushing[id]; carried {
		done = c.flushDone
	}
--- replace
	if _, carried := c.flushing[id]; carried || true {
		done = c.flushDone
	}
--- end

[the flush is announced complete before its write returns]
file internal/checkpoint/checkpointer.go
--- anchor
	defer func() {
		c.mu.Lock()
		c.flushDone = nil
		c.flushing = nil
		c.mu.Unlock()
		close(done)
	}()
--- replace
	c.mu.Lock()
	c.flushDone = nil
	c.flushing = nil
	c.mu.Unlock()
	close(done)
--- end

[Prune answers "is a flush writing this" from the map it just cleared]
file internal/checkpoint/checkpointer.go
--- anchor
	delete(c.dirty, id)
	delete(c.inFlight, id)
	var done chan struct{}
	if _, carried := c.flushing[id]; carried {
--- replace
	delete(c.dirty, id)
	_, wasInFlight := c.inFlight[id]
	delete(c.inFlight, id)
	var done chan struct{}
	if carried := wasInFlight; carried {
--- end
