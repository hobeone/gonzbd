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

[persistIfChanged stops reading the release reason back, so a restored job's stored reason is overwritten with empty]
file internal/dispatch/tick.go
--- anchor
	p.Par2ReleaseReason = j.Par2ReleaseReason()
--- replace
	p.Par2ReleaseReason = ""
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
