pkg ./internal/dispatch/
run TestAdd_PersistFailureUnwindsWithoutTickTouchingJob|TestAdd_PostPersistKickWakesTickAfterBlockedSave

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
	d.kick() // register's kick may have been spent on a tick that skipped j
--- replace
	// d.kick() neutered
--- end
