pkg ./internal/dispatch/
run TestAdd_RefusedBeforeRestore|TestAdd_RefusedWhileRestoring|TestStart_AfterRestoreKeepsStoredRowsAheadOfAddedJobs|TestRestore_RefusedWhenAlreadyRestoredOrStopped|TestRestore_RollsBackWhenALaterRowFails|TestAdd_RejectsADuplicateID|TestRestoreHelper_PhaseBranches|TestRestore_StoppedDuringRestoreIsReported

[Add admits a job before the stored rows are restored]
file internal/dispatch/registry.go
--- anchor
	case d.phase != phaseRestored:
--- replace
	case false:
--- end

[Add admits a job while restore is in progress]
file internal/dispatch/registry.go
--- anchor
	case d.phase != phaseRestored:
--- replace
	case d.phase == phaseUnrestored:
--- end

[Start refuses a Dispatcher Restore already restored]
file internal/dispatch/dispatch.go
--- anchor
	case d.phase == phaseRestored && reuse:
--- replace
	case false && reuse:
--- end

[Restore restores a second time]
file internal/dispatch/dispatch.go
--- anchor
	case d.phase == phaseRestored && reuse:
--- replace
	case d.phase == phaseRestored:
--- end

[a restore admitted while another is in progress]
file internal/dispatch/dispatch.go
--- anchor
	case d.phase == phaseRestoring:
--- replace
	case false:
--- end

[Restore admitted on a stopped Dispatcher]
file internal/dispatch/dispatch.go
--- anchor
	switch {
	case d.stopped:
		d.mu.Unlock()
		return errors.New("this Dispatcher is stopped")
--- replace
	switch {
	case false:
		d.mu.Unlock()
		return errors.New("this Dispatcher is stopped")
--- end

[a successful restore never reaches the restored phase]
file internal/dispatch/dispatch.go
--- anchor
	settled = phaseRestored
--- replace
	settled = phaseUnrestored
--- end

[a failed Store.Load leaves the Dispatcher restored]
file internal/dispatch/dispatch.go
--- anchor
	settled := phaseUnrestored
--- replace
	settled := phaseRestored
--- end

[Restore reports success after a Stop landed inside it]
file internal/dispatch/dispatch.go
--- anchor
	if stopped {
		return errors.New("dispatch: Restore: stopped while restoring; this Dispatcher is terminal")
--- replace
	if stopped && false {
		return errors.New("dispatch: Restore: stopped while restoring; this Dispatcher is terminal")
--- end
