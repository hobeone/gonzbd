pkg ./internal/dispatch/
run TestEvictCancelledNeverRun_Refuses|TestEvictCancelledNeverRun_StoreFailureReadmitsWork|TestEvictCancelledNeverRun_SkipsWhenLiveOrLaunched|TestRemove_RefcountsConcurrentRemovals|TestDeregister_IsTotal|TestBeginRemovalIfIdle_Outcomes|TestAdmitsLocked_TruthTable

[the removal marker dropped from the admission predicate]
file internal/dispatch/registry.go
--- anchor
	return d.byID[id] != nil && d.removing[id] == 0
--- replace
	return d.byID[id] != nil
--- end

[the marker is never raised, so every path deregisters unprotected — #513 itself]
file internal/dispatch/registry.go
--- anchor
	d.removing[id]++
	return &removal{d: d, id: id}
--- replace
	d.removing[id] += 0
	return &removal{d: d, id: id}
--- end

[the liveness check split back out of the marker's lock span]
file internal/dispatch/registry.go
--- anchor
	if len(d.occupancyTokens[id]) > 0 || d.launched[id] != nil {
		return nil, true
	}
--- replace
	if false {
		return nil, true
	}
--- end

[abort stops releasing the marker, leaving a registered job inert]
file internal/dispatch/registry.go
--- anchor
	r.d.removing[r.id]--
--- replace
	r.d.removing[r.id] -= 0
--- end

[abort loses its idempotence guard, so a finished removal decrements a live one]
file internal/dispatch/registry.go
--- anchor
func (r *removal) abort() {
	if r == nil || r.done {
--- replace
func (r *removal) abort() {
	if r == nil {
--- end

[deregister stops clearing one of its nine per-job maps]
file internal/dispatch/registry.go
--- anchor
	delete(d.written, id)
--- replace
	delete(d.byID, id)
--- end

[a refused beginRemovalIfIdle leaves its marker behind]
file internal/dispatch/registry.go
--- anchor
	if len(d.occupancyTokens[id]) > 0 || d.launched[id] != nil {
		return nil, true
	}
--- replace
	if len(d.occupancyTokens[id]) > 0 || d.launched[id] != nil {
		d.removing[id]++
		return nil, true
	}
--- end
