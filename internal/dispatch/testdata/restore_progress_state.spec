pkg ./internal/dispatch/
run TestRestore_DoesNotRewriteRowsItJustRead

[the reason fallback removed, so a non-hydrated job reads empty and the row is rewritten]
file internal/job/content.go
--- anchor
	if j.progress == nil {
		return j.restoredPar2Reason
	}
--- replace
	if j.progress == nil {
		return ""
	}
--- end

[the download-start fallback removed]
file internal/job/content.go
--- anchor
	if j.progress == nil {
		return j.restoredDLStarted
	}
--- replace
	if j.progress == nil {
		return time.Time{}
	}
--- end

[the download-finish fallback removed]
file internal/job/content.go
--- anchor
	if j.progress == nil {
		return j.restoredDLFinished
	}
--- replace
	if j.progress == nil {
		return time.Time{}
	}
--- end

[persistIfChanged stops reading the par2Recovered flag back, so a restored job's stored true is overwritten with false]
file internal/dispatch/tick.go
--- anchor
	p.Par2Recovered = j.Par2Recovered()
--- replace
	p.Par2Recovered = false
--- end

[the Job accessor loses its pre-hydration fallback, so a never-hydrated job reports false]
file internal/job/content.go
--- anchor
	if j.progress == nil {
		return j.restoredPar2Recovered
	}
--- replace
	if j.progress == nil {
		return false
	}
--- end

[RestoreProgressState stops recording the reason, as the old silent no-op did]
file internal/job/content.go
--- anchor
	j.restoredPar2Reason = reason
	j.restoredDLStarted = jobStampOrZero(started)
--- replace
	j.restoredPar2Reason = ""
	j.restoredDLStarted = jobStampOrZero(started)
--- end
