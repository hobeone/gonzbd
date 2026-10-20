pkg ./internal/app/
run TestAppResidency_HydrateWaitsForAHydrationInFlight|TestAppResidency_HydrateRefusesAProgressRecordOfAnotherShape

[a waiter reports the failure of the hydration it waited on as its own]
file internal/app/residency.go
--- anchor
		if err := r.hydrating.Wait(ctx, id); err != nil {
			return err
		}
--- replace
		if err := r.hydrating.Wait(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("residency: hydrate %s: concurrent hydration failed", id)
--- end

[a waiter never waits on the hydration in flight]
file internal/app/residency.go
--- anchor
		if err := r.hydrating.Wait(ctx, id); err != nil {
--- replace
		if err := ctx.Err(); err != nil {
--- end

[a waiter ignores its cancelled context]
file internal/app/residency.go
--- anchor
		if err := r.hydrating.Wait(ctx, id); err != nil {
			return err
		}
--- replace
		if err := r.hydrating.Wait(ctx, id); err != nil {
			return nil
		}
--- end

[a re-hydration ignores an article list its file table refuses]
file internal/app/residency.go
--- anchor
	m, err := job.NewManifestFromArticles(ft, arts)
	if err != nil {
		return fmt.Errorf("residency: hydrate %s: %w", id, err)
	}
--- replace
	m, err := job.NewManifestFromArticles(ft, arts)
	if err != nil {
		return nil
	}
--- end
