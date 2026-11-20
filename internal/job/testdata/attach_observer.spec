pkg ./internal/job/
run ^Test(AttachObserver_RunsOnlyForASuccessfulAttach|SetAttachObserver_NilUninstalls)$

[a nil observer is stored as a nil function]
file internal/job/content.go
--- anchor
	if fn == nil {
		j.attachObserver.Store(nil)
		return
	}
--- replace
--- end

[the observer runs for a refused attach too]
file internal/job/content.go
--- anchor
		if attached {
			j.notifyAttached()
		}
--- replace
		_ = attached
		j.notifyAttached()
--- end
