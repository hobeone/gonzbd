pkg ./internal/app/
run TestAppResidency_HydrateThenEvict

[hydration always attaches fresh progress, zeroing a re-hydrated job's counters]
file internal/app/residency.go
--- anchor
	if p := j.Progress(); p != nil {
		if err := j.RestoreContent(m, p); err != nil {
			return err
		}
	} else if err := j.AttachContent(m); err != nil {
--- replace
	if false {
	} else if err := j.AttachContent(m); err != nil {
--- end

