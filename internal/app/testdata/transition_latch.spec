pkg ./internal/app/
run TestJobTransitions_(TryAcquireTakesTheFreeIDsAndSkipsTheHeld|AWokenWaiterRechecksBeforeTakingTheID|ReleaseRemovesTheIDs|RemovedForgetsACollectedJob|AcquireTakesAFreeIDOnAnEndedContext)$

# jobTransitions' own exclusion, separately from its call sites
# (transition_lock.spec). The claim and release themselves are
# keyedlatch.Latch's, pinned by internal/keyedlatch/testdata/latch.spec; these
# mutations neuter jobTransitions' use of them.

[an ended context refuses a free id]
file internal/app/transition.go
--- anchor
	for !t.claim(id) {
--- replace
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	for !t.claim(id) {
--- end

[tryAcquire takes an id another claim holds]
file internal/app/transition.go
--- anchor
		if t.held.TryClaim(id) {
--- replace
		if t.held.TryClaim(id) || true {
--- end

[a woken waiter takes the id without re-checking]
file internal/app/transition.go
--- anchor
		if err := t.held.Wait(ctx, id); err != nil {
			return nil, err
		}
--- replace
		if err := t.held.Wait(ctx, id); err != nil {
			return nil, err
		}
		break
--- end

[release leaves its ids held]
file internal/app/transition.go
--- anchor
		c.t.held.Release(id)
--- replace
		c.t.held.Release("mut-" + id)
--- end

[a removal mark outlives its job]
file internal/app/transition.go
--- anchor
	defer t.mu.Unlock()
	delete(t.removed, key)
--- replace
	defer t.mu.Unlock()
	_ = key
--- end
