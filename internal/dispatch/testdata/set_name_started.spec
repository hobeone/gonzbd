pkg ./internal/dispatch/
run TestSetName_RefusesAJobThatHasStarted

[a started job may be renamed]
file internal/dispatch/registry.go
--- anchor
	if e.j.DownloadBegun() {
--- replace
	if false {
--- end

[a job with a registered file (failed or retried) may be renamed]
file internal/job/content.go
--- anchor
		if j.progress.files[i].Filename != "" {
--- replace
		if j.progress.files[i].Filename != "" && i < 0 {
--- end

[a job with no registered file (an attempt, an unregistered failure) is refused]
file internal/job/content.go
--- anchor
		if j.progress.files[i].Filename != "" {
			return true
		}
	}
	return false
--- replace
		if j.progress.files[i].Filename != "" {
			return true
		}
	}
	return true
--- end
