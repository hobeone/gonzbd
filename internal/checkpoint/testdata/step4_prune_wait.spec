pkg ./internal/checkpoint/
run TestPrune_|TestCheckpointer_
timeout 3m

[Prune does not wait for the flush carrying the job]
file internal/checkpoint/checkpointer.go
--- anchor
	if carried {
		done = c.flushDone
	}
--- replace
	_ = carried
--- end

[Prune waits for every flush, not only the one carrying the job]
file internal/checkpoint/checkpointer.go
--- anchor
	if carried {
		done = c.flushDone
	}
--- replace
	_ = carried
	done = c.flushDone
--- end

[the flush is announced complete before its write returns]
file internal/checkpoint/checkpointer.go
--- anchor
	defer func() {
		c.mu.Lock()
		c.flushDone = nil
		c.mu.Unlock()
		close(done)
	}()
--- replace
	c.mu.Lock()
	c.flushDone = nil
	c.mu.Unlock()
	close(done)
--- end
