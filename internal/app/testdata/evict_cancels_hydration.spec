pkg ./internal/app/
run TestAppResidency_EvictEndsAHydrationBlockedInItsManifestRead|TestAppResidency_FaultAfterCancellationIsAContextError

[Evict waits on the hydration without cancelling it]
file internal/app/residency.go
--- anchor
		h.cancel()
--- replace
		_ = h.cancel
--- end

[a hydration registers no cancel for Evict to call]
file internal/app/residency.go
--- anchor
		r.hydrating[id] = hydration{ready: ready, cancel: cancel}
--- replace
		r.hydrating[id] = hydration{ready: ready, cancel: func() {}}
--- end

[the manifest read does not return at the cancellation]
file internal/app/residency.go
--- anchor
	case <-ctx.Done():
		return nil, ctx.Err()
	}
--- replace
	case <-make(chan struct{}):
		return nil, ctx.Err()
	}
--- end

[an error after the cancellation is not reported as a context error]
file internal/app/residency.go
--- anchor
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
--- replace
	if ctxErr := ctx.Err(); false && ctxErr != nil && !errors.Is(err, ctxErr) {
--- end
