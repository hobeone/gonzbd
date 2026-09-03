pkg ./internal/app/
run TestAppResidency_HydrateThenEvict

[hydration always attaches fresh progress, zeroing a re-hydrated job's counters]
file internal/app/residency.go
--- anchor
	if p := j.Progress(); p != nil {
		return j.RestoreContent(m, p)
	}
--- replace
	if false {
	}
--- end
