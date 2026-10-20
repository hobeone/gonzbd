pkg ./internal/app/
run TestAppResidency_HydrateThenEvict

[hydration always attaches fresh progress, zeroing a re-hydrated job's counters]
file internal/app/residency.go
--- anchor
	if hasFiles {
		return j.AttachArticles(m)
	}
--- replace
	if false {
		return j.AttachArticles(m)
	}
--- end

