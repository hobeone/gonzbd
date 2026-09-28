pkg ./internal/app/
run Test(RemoveJob_DuringTheDirectUnpackWait_PostProcessingDoesNotRun|RemoveJob_InterruptsTheDirectUnpackWait|EnqueuePostProc_AfterRemoveJob_DoesNotHandOver|PostProcAdmissions_HandOverRefusesARemovedInstance|PostProcAdmissions_WithdrawWaitsOutAHandOver|PostProcAdmissions_WithdrawIsSafeToRepeat|AwaitDirectUnpackOrAbort_RemovalAbortsAndContinues)$

# A removed job's post-processing does not start: the hand-over refuses an
# instance RemoveJob marked, RemoveJob's withdraw ends the DirectUnpack wait
# and waits out a hand-over in progress, and a refused enqueue ends its
# admission.

[the hand-over ignores the removal mark]
file internal/app/postproc_admission.go
--- anchor
	if !ok || t.wasRemoved(j) {
--- replace
	if !ok {
--- end

[the enqueue hands over a job the hand-over refused]
file internal/app/app.go
--- anchor
		if !handing {
--- replace
		if !handing && false {
--- end

[a refused enqueue keeps its admission]
file internal/app/app.go
--- anchor
			app.postProcAdmissions.release(j)
			return
--- replace
			return
--- end

[RemoveJob does not withdraw the admission]
file internal/app/app.go
--- anchor
	app.postProcAdmissions.withdraw(j)
--- replace
	_ = j
--- end

[withdraw does not close the removal channel]
file internal/app/postproc_admission.go
--- anchor
		close(cur.removed)
--- replace
		_ = cur.removed
--- end

[removal hands the wait no channel]
file internal/app/postproc_admission.go
--- anchor
		return cur.removed
--- replace
		_ = cur
		return nil
--- end

[the DirectUnpack wait ignores a removal]
file internal/app/app.go
--- anchor
	case <-removed:
		du.Abort()
		<-waited
		return true
--- replace
	case <-(<-chan struct{})(nil):
		du.Abort()
		<-waited
		return true
--- end

[a removal skips the hand-over as a shutdown does]
file internal/app/app.go
--- anchor
	case <-removed:
		du.Abort()
		<-waited
		return true
--- replace
	case <-removed:
		du.Abort()
		<-waited
		return false
--- end

[withdraw does not wait out a hand-over in progress]
file internal/app/postproc_admission.go
--- anchor
	if handing != nil {
		<-handing
--- replace
	if false {
		<-handing
--- end

[endHandOver does not release a waiting withdraw]
file internal/app/postproc_admission.go
--- anchor
	if cur, ok := a.jobs[j]; ok && cur.handing != nil {
		close(cur.handing)
		cur.handing = nil
--- replace
	if cur, ok := a.jobs[j]; ok && cur.handing != nil {
		cur.handing = nil
--- end

[release leaves an open hand-over's withdraw waiting]
file internal/app/postproc_admission.go
--- anchor
		close(cur.handing)
	}
	delete(a.jobs, j)
--- replace
	}
	delete(a.jobs, j)
--- end
