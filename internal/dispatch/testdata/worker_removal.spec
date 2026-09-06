pkg ./internal/dispatch/
run TestRemove_WaitsForActiveWorkerBeforeEvicting

[neutering waitLaunched in Remove]
file internal/dispatch/registry.go
--- anchor
	if err := d.waitLaunched(ctx, id); err != nil {
		d.mu.Lock()
		d.removing[id]--
		if d.removing[id] <= 0 {
			delete(d.removing, id)
		}
		d.mu.Unlock()
		return fmt.Errorf("dispatch: remove %s: wait worker: %w", id, err)
	}
--- replace
	if false {
		return nil
	}
--- end
