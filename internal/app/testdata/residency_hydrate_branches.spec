pkg ./internal/app/
run TestAppResidency_HydrateWaitsForAHydrationInFlight|TestAppResidency_HydrateRefusesAProgressRecordOfAnotherShape

[a waiter reports the failure of the hydration it waited on as its own]
file internal/app/residency.go
--- anchor
			select {
			case <-inFlight:
				continue
			case <-ctx.Done():
--- replace
			select {
			case <-inFlight:
				return fmt.Errorf("residency: hydrate %s: concurrent hydration failed", id)
			case <-ctx.Done():
--- end

[a waiter ignores its cancelled context]
file internal/app/residency.go
--- anchor
			case <-ctx.Done():
				return ctx.Err()
			}
--- replace
			case <-ctx.Done():
				return nil
			}
--- end

[re-hydration ignores a refused restore]
file internal/app/residency.go
--- anchor
		return j.RestoreContent(m)
--- replace
		_ = j.RestoreContent(m)
		return nil
--- end
