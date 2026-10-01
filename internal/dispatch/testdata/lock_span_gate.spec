pkg ./internal/dispatch/
run TestNoCallIntoQueueUnderDispatcherLock

[a direct sched.Queue call reaches under a held d.mu span]
file internal/dispatch/tick.go
--- anchor
	delete(d.resident, id)
--- replace
	d.q.Advance(nil)
	delete(d.resident, id)
--- end

[a call to a Queue-reaching helper reaches under a held d.mu span]
file internal/dispatch/tick.go
--- anchor
	return d.resident[id]
--- replace
	d.parkGrant(nil)
	return d.resident[id]
--- end
