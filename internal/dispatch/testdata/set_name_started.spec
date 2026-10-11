pkg ./internal/dispatch/
run TestSetName_RefusesAJobThatHasStarted

[a started job may be renamed]
file internal/job/job.go
--- anchor
	if j.DownloadBegun() { // takes contentMu, the inner lock
--- replace
	if false {
--- end

[a started job's refusal is not ErrJobStarted]
file internal/dispatch/registry.go
--- anchor
		return fmt.Errorf("dispatch: set name %s: %w: %w", id, ErrJobStarted, err)
--- replace
		return fmt.Errorf("dispatch: set name %s: %w", id, err)
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
