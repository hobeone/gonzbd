pkg ./internal/app/
run TestAppResidency_HydrateThenEvict

[hydration always attaches fresh progress, zeroing a re-hydrated job's counters]
file internal/app/residency.go
--- anchor
	if j.HasProgress() {
		return j.RestoreContent(m)
	}
--- replace
	if false {
		return j.RestoreContent(m)
	}
--- end

