pkg ./internal/dispatch/
run TestResidency_AbortedRemoveDuringHydrateIsEvictedByTheNextTick|TestResidency_SuccessfulRemoveDuringHydrateLeavesNothingResident|TestRemovingState_SuppressesPersistAndLaunch

# markResident records a load while a removal is outstanding, and a successful
# removal still leaves no manifest and no residency record.

[markResident refuses again while a removal is outstanding]
file internal/dispatch/tick.go
--- anchor
	if d.byID[id] == nil {
		return
	}
	d.resident[id] = true
--- replace
	if !d.admitsLocked(id) {
		return
	}
	d.resident[id] = true
--- end

[markResident records an unregistered id]
file internal/dispatch/tick.go
--- anchor
	if d.byID[id] == nil {
		return
	}
	d.resident[id] = true
--- replace
	if false {
		return
	}
	d.resident[id] = true
--- end

[a successful Remove no longer evicts the manifest]
file internal/dispatch/registry.go
--- anchor
	d.res.Evict(id)
	rm.end()
--- replace
	rm.end()
--- end

[deregister no longer clears the residency record]
file internal/dispatch/registry.go
--- anchor
	delete(d.resident, id)
--- replace
	_ = id
--- end
