pkg ./internal/app/
run Test(Stall_Admission|PostProcAdmissions_UnlessHandedOff|PostProcAdmissions_NoReasonAfterTheNotesAreTaken)$

# A stall record is written only for an instance that is neither admitted to
# post-processing nor past its admission; a fault on one that is goes to the
# run, if one is left. Each half of the writer's gate is removed on its own,
# then the routing of a refused fault.

[the stall record is written whatever the admission]
file internal/app/stall.go
--- anchor
	return app.postProcAdmissions.unlessHandedOff(j, func() {
--- replace
	return func(fn func()) bool { fn(); return true }(func() {
--- end

[unlessHandedOff runs fn for an ended instance]
file internal/app/postproc_admission.go
--- anchor
	if _, ok := a.jobs[j]; ok || a.endedLocked(j) {
--- replace
	if _, ok := a.jobs[j]; ok {
--- end

[a verify fault refused by the gate is not offered to the run]
file internal/app/durability.go
--- anchor
		app.offerFaultToAdmission(j, f, kind.source())
--- replace
		if kind != stallVerify {
			app.offerFaultToAdmission(j, f, kind.source())
		}
--- end

[a refused fault is not paused]
file internal/app/durability.go
--- anchor
		app.offerFaultToAdmission(j, f, kind.source())
	}
--- replace
		app.offerFaultToAdmission(j, f, kind.source())
		return
	}
--- end

[a reason after the notes were taken is noted and dropped]
file internal/app/postproc_admission.go
--- anchor
		if cur.notesTaken {
			return refusedEnded
		}
--- replace
		if false {
			return refusedEnded
		}
--- end

[the notes-taken check runs before the duplicate check]
file internal/app/postproc_admission.go
--- anchor
		if failMsg == "" || failMsg == cur.failMsg || slices.Contains(cur.notes, failMsg) {
			return refused
		}
		if cur.notesTaken {
			return refusedEnded
		}
--- replace
		if cur.notesTaken {
			return refusedEnded
		}
		if failMsg == "" || failMsg == cur.failMsg || slices.Contains(cur.notes, failMsg) {
			return refused
		}
--- end

[closeAndTakeNotes leaves the admission open to reasons]
file internal/app/postproc_admission.go
--- anchor
	cur.notesTaken = true
--- replace
	_ = cur
--- end

[a fault on an ended instance is admitted to a new run]
file internal/app/postproc_admission.go
--- anchor
func (a *postProcAdmissions) admitLocked(j *job.Job, failMsg string) admitOutcome {
	if a.endedLocked(j) {
--- replace
func (a *postProcAdmissions) admitLocked(j *job.Job, failMsg string) admitOutcome {
	if false {
--- end
