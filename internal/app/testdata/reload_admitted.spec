pkg ./internal/app/
run Test(ReloadDownloader_LeavesAnAdmittedJobsProgressAlone|PostProcAdmissions_UnlessHandedOff)$

[the reload clears an admitted job]
file internal/app/reloader.go
--- anchor
				app.postProcAdmissions.unlessHandedOff(j, func() { j.ClearEmittedForReload(false) })
--- replace
				j.ClearEmittedForReload(false)
--- end

[unlessHandedOff ignores the admission]
file internal/app/postproc_admission.go
--- anchor
	if _, ok := a.jobs[j]; ok || a.endedLocked(j) {
--- replace
	if _, ok := a.jobs[j]; (ok && false) || a.endedLocked(j) {
--- end

[unlessHandedOff releases the lock across fn]
file internal/app/postproc_admission.go
--- anchor
	fn()
	return true
--- replace
	a.mu.Unlock()
	fn()
	a.mu.Lock()
	return true
--- end
