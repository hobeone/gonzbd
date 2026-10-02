pkg ./internal/app/
run TestAppResidency_HydrateWaitsForAHydrationInFlight|TestAppResidency_HydrateRefusesAProgressRecordOfAnotherShape

[a waiter reports success for a hydration that failed]
file internal/app/residency.go
--- anchor
		if !j.Resident() {
			return fmt.Errorf("residency: hydrate %s: concurrent hydration failed", id)
		}
--- replace
		if false {
			return fmt.Errorf("residency: hydrate %s: concurrent hydration failed", id)
		}
--- end

[a waiter ignores its cancelled context]
file internal/app/residency.go
--- anchor
		case <-ctx.Done():
			return ctx.Err()
		}
		if !j.Resident() {
--- replace
		case <-ctx.Done():
			return nil
		}
		if !j.Resident() {
--- end

[re-hydration ignores a refused restore]
file internal/app/residency.go
--- anchor
		if err := j.RestoreContent(m); err != nil {
			return err
		}
--- replace
		if err := j.RestoreContent(m); err != nil {
			return nil
		}
--- end
