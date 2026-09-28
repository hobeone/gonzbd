pkg ./internal/app/
run TestJobTransitions_(TryAcquireTakesTheFreeIDsAndSkipsTheHeld|AWokenWaiterRechecksBeforeTakingTheID|ReleaseRemovesTheIDs)$

# jobTransitions' own exclusion, separately from its call sites
# (transition_lock.spec). The run line leaves out the several-waiters test,
# which a re-check mutant kills by closing a nil channel rather than on an
# assertion.

[a claim takes an id another claim holds]
file internal/app/transition.go
--- anchor
	if _, busy := t.held[id]; busy {
--- replace
	if _, busy := t.held[id]; busy && false {
--- end

[a woken waiter takes the id without re-checking]
file internal/app/transition.go
--- anchor
		case <-holder:
--- replace
		case <-holder:
			return &transitionClaim{t: t, ids: map[string]struct{}{id: {}}}, nil
--- end

[release leaves its ids in the map]
file internal/app/transition.go
--- anchor
		delete(c.t.held, id)
--- replace
		delete(c.t.held, "mut-"+id)
--- end
