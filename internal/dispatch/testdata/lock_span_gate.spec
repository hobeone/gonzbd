pkg ./internal/dispatch/
run TestNoCallIntoQueueUnderDispatcherLock|TestLockSpanGateClassifiesKnownShapes

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

[classifyMember's catch-all for an unenumerated two-level field call reverted to callIgnored]
file internal/dispatch/lock_span_gate_test.go
--- anchor
			return callUnresolved, "d." + rest[0] + "." + rest[1] + "(...)"
--- replace
			return callIgnored, ""
--- end

[transitive propagation of an unresolved call through a resolved local helper dropped]
file internal/dispatch/lock_span_gate_test.go
--- anchor
			for _, u := range analysis.unresolvedReach[ident] {
--- replace
			for _, u := range []string(nil) {
--- end

[the allow-list pointer dropped from an unresolved call's violation message]
file internal/dispatch/lock_span_gate_test.go
--- anchor
			"    Move the call out of the span, or add %q to queueCallAllow with the reason it cannot reach sched.Queue.",
--- replace
			"    Move the call out of the span, or add %q to the allow map with the reason it cannot reach sched.Queue.",
--- end
