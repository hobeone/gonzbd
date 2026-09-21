pkg ./internal/dispatch/
run TestDeregister_LeavesAnotherRemovalsMarkerStanding|TestDeregister_IsTotal|TestAdd_RemovalBeforeTheWriteIsNotReportedAsSuccess|TestAdd_PersistFailureUnwindsWithoutTickTouchingJob|TestAdd_PostPersistKickWakesTickAfterBlockedSave|TestAdd_AbortedRemovalDuringTheWriteLeavesTheJobVisible|TestRemove_OverlappingAddWithTransientDeleteErrorIsRetriedByTick|TestStart_RefusesARowThatDiffersFromTheOneAddWrote|TestAdd_SingleKickOnlyAfterPersist|TestAdd_WritesUnderTheCallersContext|TestRemovingState_SuppressesPersistAndResidency|TestRegister_RefusesWhileARemovalIsOutstanding

[snapshotOrder written gate neutered]
file internal/dispatch/registry.go
--- anchor
		if _, ok := d.written[id]; !ok { // read under the held d.mu
			continue
		}
--- replace
		if false {
			continue
		}
--- end

[markWritten re-gated on admitsLocked, so an outstanding removal suppresses it]
file internal/dispatch/dispatch.go
--- anchor
	if d.byID[p.ID] == nil {
		return
	}
--- replace
	if !d.admitsLocked(p.ID) {
		return
	}
--- end

[markWritten's registration check neutered, so a deregistered id can be recorded]
file internal/dispatch/dispatch.go
--- anchor
	if d.byID[p.ID] == nil {
		return
	}
--- replace
	if false {
		return
	}
--- end

[Add unwind gatekeeper neutered]
file internal/dispatch/registry.go
--- anchor
		if rm, ok := d.beginRemoval(j.ID()); ok {
			rm.end()
		}
--- replace
		if rm, ok := (*removal)(nil), false; ok {
			rm.end()
		}
--- end

[Add post-persist kick neutered]
file internal/dispatch/registry.go
--- anchor
	d.kick() // register skips its kick for seqNext while j is unwritten; wake the tick now that d.written admits j
--- replace
	// d.kick() neutered
--- end

[register single-kick guard neutered]
file internal/dispatch/registry.go
--- anchor
	if !added {
		d.kick()
	}
--- replace
	if true {
		d.kick()
	}
--- end

[restore value equality check neutered]
file internal/dispatch/dispatch.go
--- anchor
		if w, ok := d.lastWritten(p.ID); ok && w == p {
--- replace
		if _, ok := d.lastWritten(p.ID); ok {
--- end

[Add's check that its own write landed neutered]
file internal/dispatch/registry.go
--- anchor
		if _, ok := d.lastWritten(j.ID()); !ok {
			err = errPreemptedByRemoval
		}
--- replace
		if false {
			err = errPreemptedByRemoval
		}
--- end

[deregister wipes the removal marker instead of decrementing it]
file internal/dispatch/registry.go
--- anchor
	d.removing[id]--
	if d.removing[id] <= 0 {
		delete(d.removing, id)
	}
--- replace
	delete(d.removing, id)
--- end

[register's outstanding-removal refusal neutered]
file internal/dispatch/registry.go
--- anchor
	if d.removing[j.ID()] > 0 {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: register: %s: %w", j.ID(), errPreemptedByRemoval)
	}
--- replace
	if false {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: register: %s: %w", j.ID(), errPreemptedByRemoval)
	}
--- end
