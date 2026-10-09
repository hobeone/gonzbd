pkg ./internal/dispatch/store/
run TestStore_LoadRejectsAnOutOfRangeEnum$

[Store.Load returns an error on scan failure instead of skipping the row]
file internal/dispatch/store/store.go
--- anchor
		); err != nil {
			s.log.Error("dispatch/store: load scan failed", "job_id", p.ID, "err", err)
			continue
		}
--- replace
		); err != nil {
			s.log.Error("dispatch/store: load scan failed", "job_id", p.ID, "err", err)
			return nil, fmt.Errorf("dispatch/store: load: scan: %w", err)
		}
--- end

[Store.Load skips logging the scan error]
file internal/dispatch/store/store.go
--- anchor
		); err != nil {
			s.log.Error("dispatch/store: load scan failed", "job_id", p.ID, "err", err)
			continue
		}
--- replace
		); err != nil {
			if false {
				s.log.Error("dispatch/store: load scan failed", "job_id", p.ID, "err", err)
			}
			continue
		}
--- end

[Store.Load appends the partially scanned row instead of continuing]
file internal/dispatch/store/store.go
--- anchor
		); err != nil {
			s.log.Error("dispatch/store: load scan failed", "job_id", p.ID, "err", err)
			continue
		}
--- replace
		); err != nil {
			s.log.Error("dispatch/store: load scan failed", "job_id", p.ID, "err", err)
		}
--- end

[Store.Has reports false for a present row]
file internal/dispatch/store/store.go
--- anchor
	if err != nil {
		return false, fmt.Errorf("dispatch/store: has %s: %w", id, err)
	}
	return true, nil
--- replace
	if err != nil {
		return false, fmt.Errorf("dispatch/store: has %s: %w", id, err)
	}
	return false, nil
--- end

[Store.Has reports true when sql.ErrNoRows is returned]
file internal/dispatch/store/store.go
--- anchor
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
--- replace
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
--- end
