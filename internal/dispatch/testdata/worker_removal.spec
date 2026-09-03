pkg ./internal/dispatch/
run TestRemove_WaitsForActiveWorkerBeforeEvicting

[neutering waitLaunched in Remove]
file internal/dispatch/registry.go
--- anchor
	if err := d.waitLaunched(ctx, id); err != nil {
		return fmt.Errorf("dispatch: remove %s: wait worker: %w", id, err)
	}
--- replace
	if false {
		return nil
	}
--- end
