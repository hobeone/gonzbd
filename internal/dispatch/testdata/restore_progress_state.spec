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

[RestoreProgressState stops recording the reason, as the old silent no-op did]
file internal/job/content.go
--- anchor
func (j *Job) RestoreProgressState(reason string, started, finished time.Time) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.restoredPar2Reason = reason
--- replace
func (j *Job) RestoreProgressState(reason string, started, finished time.Time) {
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	j.restoredPar2Reason = ""
--- end
