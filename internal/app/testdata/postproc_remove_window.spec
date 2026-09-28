pkg ./internal/app/
timeout 90s
run Test(RemoveJob_DuringTheDirectUnpackWait_PostProcessingDoesNotRun|RemoveJob_WaitsForTheDirectUnpackToStop|EnqueuePostProc_RemovedBeforeItsHandOver_ReleasesTheJob|RemoveJob_InterruptsTheDirectUnpackWait|PostProcAdmissions_HandOverRefusesARemovedInstance|PostProcAdmissions_WithdrawWaitsOutAStep|PostProcAdmissions_AStaleTokenEndsNoStep|PostProcAdmissions_WithdrawIsSafeToRepeat|AwaitDirectUnpackOrAbort_RemovalAbortsAndContinues)$

# A removed job's post-processing does not start: the hand-over refuses an
# instance RemoveJob marked and hands it back through jobFinalizer.cancelled,
# and RemoveJob's withdraw aborts the DirectUnpack wait and waits for the
# enqueue's step in progress to end.

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
		if !ok {
			// A RemoveJob took the job.
--- replace
		if !ok && false {
			// A RemoveJob took the job.
--- end

[a refused hand-over only ends the admission, leaving the launch claim held]
file internal/app/app.go
--- anchor
			app.finalizer.cancelled(&postproc.Job{Job: j})
			return
--- replace
			app.postProcAdmissions.release(j)
			return
--- end

[a refused hand-over keeps its admission and its claim]
file internal/app/app.go
--- anchor
			app.finalizer.cancelled(&postproc.Job{Job: j})
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

[beginWait hands the wait no removal channel]
file internal/app/postproc_admission.go
--- anchor
	return cur.removed, cur.busy
--- replace
	return nil, cur.busy
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

[withdraw does not wait out the step in progress]
file internal/app/postproc_admission.go
--- anchor
	if busy != nil {
		<-busy
--- replace
	if false {
		<-busy
--- end

[the DirectUnpack wait is not a step withdraw waits for]
file internal/app/app.go
--- anchor
	removed, wait := app.postProcAdmissions.beginWait(j)
--- replace
	removed, wait := app.postProcAdmissions.beginWait(j)
	app.postProcAdmissions.endStep(j, wait)
--- end

[the wait step ends before the unpacker has stopped]
file internal/app/app.go
--- anchor
			finished := awaitDirectUnpackOrAbort(app.ctx, removed, du)
			app.postProcAdmissions.endStep(j, wait)
--- replace
			app.postProcAdmissions.endStep(j, wait)
			finished := awaitDirectUnpackOrAbort(app.ctx, removed, du)
--- end

[endStep does not release a waiting withdraw]
file internal/app/postproc_admission.go
--- anchor
		close(cur.busy)
		cur.busy = nil
--- replace
		cur.busy = nil
--- end

[endStep ends whatever step is current, not its own]
file internal/app/postproc_admission.go
--- anchor
	if cur, ok := a.jobs[j]; ok && token != nil && cur.busy == token {
--- replace
	if cur, ok := a.jobs[j]; ok && cur.busy != nil {
--- end

[release leaves a waiting withdraw waiting]
file internal/app/postproc_admission.go
--- anchor
	if cur, ok := a.jobs[j]; ok && cur.busy != nil {
		close(cur.busy)
	}
	delete(a.jobs, j)
--- replace
	delete(a.jobs, j)
--- end
