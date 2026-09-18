pkg ./internal/dispatch/
run TestAdd_PersistFailureUnwindsWithoutTickTouchingJob|TestAdd_PostPersistKickWakesTickAfterBlockedSave|TestAdd_AbortedRemovalDuringTheWriteLeavesTheJobVisible|TestRemove_OverlappingAddWithTransientDeleteErrorIsRetriedByTick|TestStart_RefusesARowThatDiffersFromTheOneAddWrote|TestAdd_SingleKickOnlyAfterPersist

[snapshotOrder written gate neutered]
file internal/dispatch/registry.go
--- anchor
		if _, ok := d.written[id]; !ok && e.adding { // read under the held d.mu
			continue
		}
--- replace
		if false {
			continue
		}
--- end

[snapshotOrder aborted-removal e.adding check neutered]
file internal/dispatch/registry.go
--- anchor
		if _, ok := d.written[id]; !ok && e.adding { // read under the held d.mu
			continue
		}
--- replace
		if _, ok := d.written[id]; !ok {
			continue
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
		if w, ok := d.lastWritten(p.ID); ok && w == p && !slices.Contains(registered, p.ID) {
			continue
		}
--- replace
		if _, ok := d.lastWritten(p.ID); ok && !slices.Contains(registered, p.ID) {
			continue
		}
--- end
