pkg ./internal/dispatch/
run TestSetName_RefusesARetriedJobRestoredBeforeHydration

[SetName trusts the restored row without loading progress]
file internal/dispatch/registry.go
--- anchor
	wasResident := j.Resident()
--- replace
	if ok {
		return nil
	}
	wasResident := j.Resident()
--- end

[a restored job with a recorded filename is not refused once loaded]
file internal/job/content.go
--- anchor
		if j.progress.files[i].Filename != "" {
--- replace
		if j.progress.files[i].Filename != "" && i < 0 {
--- end
