pkg ./internal/app/
run ^(TestAppResidency_EvictEndsAHydrationBlockedInItsManifestRead|TestAppResidency_EvictCancelsTheHydrationThatReplacesTheOneItCancelled)$

# Evict cancels the hydration in flight and waits for it to return before the
# job is evicted, so the verdicts a load committed reach SQLite before a
# removal's reclaim deletes the job's rows.

[a removal does not wait for the hydration in flight]
file internal/app/residency.go
--- anchor
		done := r.hydrating.Done(id)
		if done == nil {
			break
		}
--- replace
		done := r.hydrating.Done(id)
		if done == nil || true {
			break
		}
--- end
