pkg ./internal/app/
run ^(TestFail_AtAssessingWithALiveWorker_DefersToTheWorkersExit|TestFail_WithAssessingPending_DefersToTheWorkerTheTickLaunches|TestFail_OnACompleteJobAtFetching_DefersToAssessing|TestFail_BetweenTheVerdictAndItsReport_IsHandedOffAfterTheReport|TestAwaitsAssessing|TestAdmitUnlessAssessing_DefersToTheVisit|TestAdmitUnlessAssessing_AdmitsWhatIsNotAtAssessing|TestBeginAssess_AnEarlierVisitsEndTakesNothingDeferredToALaterOne|TestAdmitUnlessAssessing_AVisitNoWorkerOwnsGoesWithItsJob|TestAdmitLocked_Outcomes|TestFail_OnAStalledCompleteJobAtFetching_ResumesItForAssessing|TestFail_OnAStalledJobWithAssessingPending_ResumesItForAssessing|TestFail_OnAUserPausedJob_DefersWithoutResumingIt|TestAdmitUnlessAssessing_KeepsNoEmptyReason|TestMaybeFinalize_AnEmptyReasonAtAssessing_IsNoFailure)$
timeout 5m

# A hand-off by job ID (Fail, the hopeless callbacks) does not admit a job at
# Assessing. Its reason waits for the job's Assessing worker, which hands the
# job over in place of its verdict, or after its report for a reason that
# arrived after it last looked.

[a hand-off by ID admits a job at Assessing]
file internal/app/postproc_admission.go
--- anchor
	if _, admitted := a.jobs[j]; !admitted && awaitsAssessing(j) {
--- replace
	if false {
--- end

[maybeFinalize does not defer]
file internal/app/app.go
--- anchor
		return app.finalizeRegistered(j, failMsg, true)
--- replace
		return app.finalizeRegistered(j, failMsg, false)
--- end

[enqueuePostProc ignores the deferral]
file internal/app/app.go
--- anchor
	if deferAtAssessing {
--- replace
	if false {
--- end

[a job at Assessing is not deferred to]
file internal/app/postproc_admission.go
--- anchor
	case s.State.State == job.Assessing:
--- replace
	case s.State.State == job.Assessing && false:
--- end

[a job with Assessing pending is not deferred to]
file internal/app/postproc_admission.go
--- anchor
	case s.State.Next == job.Assessing:
--- replace
	case s.State.Next == job.Assessing && false:
--- end

[a complete job at Fetching is not deferred to]
file internal/app/postproc_admission.go
--- anchor
		return j.IsComplete()
--- replace
		return false
--- end

[a settled job is deferred to]
file internal/app/postproc_admission.go
--- anchor
	if !s.IsOpen() {
--- replace
	if false {
--- end

[a job past its verdict is deferred to]
file internal/app/postproc_admission.go
--- anchor
	case s.State.Next != job.StateUnset:
--- replace
	case s.State.Next != job.StateUnset && false:
--- end

[the Assessing worker reports its verdict over a deferred reason]
file internal/app/runner.go
--- anchor
		if reasons := r.app.postProcAdmissions.takeDeferred(j); len(reasons) > 0 {
--- replace
		if reasons := []string(nil); len(reasons) > 0 {
--- end

[a reason deferred after the worker looked is never handed over]
file internal/app/runner.go
--- anchor
	for _, reason := range r.app.postProcAdmissions.endAssess(j, visit) {
--- replace
	for _, reason := range []string(nil) {
--- end

[the Assessing worker begins no visit]
file internal/app/runner.go
--- anchor
	visit := r.app.postProcAdmissions.beginAssess(j)
--- replace
	visit := (*assessVisit)(nil)
--- end

[an earlier visit's end takes a later visit's reasons]
file internal/app/postproc_admission.go
--- anchor
	if a.assessing[key] != visit {
--- replace
	if false {
--- end

[a visit no worker owns outlives its job]
file internal/app/postproc_admission.go
--- anchor
	defer a.mu.Unlock()
	delete(a.assessing, key)
--- replace
	defer a.mu.Unlock()
	_ = key
--- end

[a new visit adopts one still owned]
file internal/app/postproc_admission.go
--- anchor
	case v.owned:
--- replace
	case false:
--- end

# A job Stall paused, which Fail then defers, is resumed by Fail so the tick
# launches the Assessing worker; a pause the user made is not.

[a deferred Fail leaves Stall's pause in place]
file internal/app/durability.go
--- anchor
	if app.maybeFinalize(jobID, reason) && parked {
--- replace
	if app.maybeFinalize(jobID, reason) && parked && false {
--- end

[a deferred Fail resumes a pause the user made]
file internal/app/durability.go
--- anchor
	if app.maybeFinalize(jobID, reason) && parked {
--- replace
	if app.maybeFinalize(jobID, reason) && (parked || true) {
--- end

[a stall record Stall did not park counts as its pause]
file internal/app/stall.go
--- anchor
	delete(app.stalls, jobID)
	return ok && rec.parked
--- replace
	delete(app.stalls, jobID)
	return ok && (rec.parked || true)
--- end

[a Fail that admits counts as deferred]
file internal/app/app.go
--- anchor
		app.log.Info("postproc: job is at Assessing; its Assessing worker hands it over",
			"job", j.ID(), "fail_msg", failMsg)
		return true
--- replace
		app.log.Info("postproc: job is at Assessing; its Assessing worker hands it over",
			"job", j.ID(), "fail_msg", failMsg)
		return false
--- end

# "" is no reason, as admitLocked reads it: deferring it would have the
# Assessing worker settle a healthy job Failed.

[an empty reason is deferred to the Assessing worker]
file internal/app/postproc_admission.go
--- anchor
		if failMsg == "" {
--- replace
		if false {
--- end
