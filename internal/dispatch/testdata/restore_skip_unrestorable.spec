pkg ./internal/dispatch/
run (TestRestore_SkipsAnUnreconstructableRow|TestRestore_SkipsARowRegisterRefuses)$

[restore returns an error on reconstruct failure instead of skipping]
file internal/dispatch/dispatch.go
--- anchor
		if err != nil {
			d.logRestoreError(p.ID, fmt.Errorf("dispatch: restore: job %s at %+v: %w", p.ID, p.State, err))
			continue
		}
--- replace
		if err != nil {
			return fmt.Errorf("dispatch: restore: job %s at %+v: %w", p.ID, p.State, err)
		}
--- end

[restore skips logging the reconstruct error]
file internal/dispatch/dispatch.go
--- anchor
		if err != nil {
			d.logRestoreError(p.ID, fmt.Errorf("dispatch: restore: job %s at %+v: %w", p.ID, p.State, err))
			continue
		}
--- replace
		if err != nil {
			continue
		}
--- end

[restore returns an error on register failure instead of skipping]
file internal/dispatch/dispatch.go
--- anchor
		if err := d.register(j, p.Header, p.SortKey); err != nil {
			d.logRestoreError(p.ID, fmt.Errorf("dispatch: restore: register %s: %w", p.ID, err))
			continue
		}
--- replace
		if err := d.register(j, p.Header, p.SortKey); err != nil {
			return fmt.Errorf("dispatch: restore: register %s: %w", p.ID, err)
		}
--- end

[restore skips logging the register error]
file internal/dispatch/dispatch.go
--- anchor
		if err := d.register(j, p.Header, p.SortKey); err != nil {
			d.logRestoreError(p.ID, fmt.Errorf("dispatch: restore: register %s: %w", p.ID, err))
			continue
		}
--- replace
		if err := d.register(j, p.Header, p.SortKey); err != nil {
			continue
		}
--- end

[restore does not advance nextSeq past a skipped row]
file internal/dispatch/dispatch.go
--- anchor
		d.mu.Lock()
		d.advanceSeqLocked(p.SortKey)
		d.mu.Unlock()
--- replace
		if false {
			d.advanceSeqLocked(p.SortKey)
		}
--- end
