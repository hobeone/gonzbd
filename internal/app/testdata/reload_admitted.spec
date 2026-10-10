pkg ./internal/app/
run Test(ReloadDownloader_LeavesAnAdmittedJobsProgressAlone|PostProcAdmissions_UnlessAdmitted)$

[the reload clears an admitted job]
file internal/app/reloader.go
--- anchor
				app.postProcAdmissions.unlessAdmitted(j, func() { j.ClearEmittedForReload(false) })
--- replace
				j.ClearEmittedForReload(false)
--- end

[unlessAdmitted ignores the admission]
file internal/app/postproc_admission.go
--- anchor
	if _, ok := a.jobs[j]; ok {
		return false
	}
	fn()
--- replace
	if false {
		return false
	}
	fn()
--- end

[unlessAdmitted releases the lock across fn]
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
