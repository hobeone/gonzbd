pkg ./internal/app/
run TestAppResidency_EvictEndsAHydrationBlockedInItsManifestRead|TestAppResidency_EvictCancelsTheHydrationThatReplacesTheOneItCancelled|TestAppResidency_FaultAfterCancellationIsAContextError|TestAppResidency_HydrateLeavesNoCancelBehind

[Evict stops after the first hydration and leaves its replacement running]
file internal/app/residency.go
--- anchor
		<-done
		r.mu.Lock()
	}
--- replace
		<-done
		r.mu.Lock()
		break
	}
--- end

[Evict waits on whatever holds the ID after mu is released, not the hydration it cancelled]
file internal/app/residency.go
--- anchor
		<-done
--- replace
		_ = done
		_ = r.hydrating.Wait(context.Background(), id)
--- end

[a finished hydration leaves its cancel behind]
file internal/app/residency.go
--- anchor
		delete(r.cancels, id)
		r.hydrating.Release(id)
--- replace
		r.hydrating.Release(id)
--- end

[Evict waits on the hydration without cancelling it]
file internal/app/residency.go
--- anchor
		r.cancels[id]()
--- replace
		_ = r.cancels[id]
--- end

[a hydration registers no cancel for Evict to call]
file internal/app/residency.go
--- anchor
			r.cancels[id] = cancel
--- replace
			r.cancels[id] = func() {}
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
