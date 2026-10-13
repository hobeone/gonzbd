pkg ./internal/app/
run ^(TestFail_WithAssessingPending_DefersToTheWorkerTheTickLaunches|TestFail_OnACompleteJobAtFetching_DefersToAssessing|TestFail_BetweenTheVerdictAndItsReport_IsHandedOffAfterTheReport|TestAwaitsAssessing|TestAdmitUnlessAssessing_DefersToTheVisit|TestBeginAssess_AnEarlierVisitsEndTakesNothingDeferredToALaterOne|TestAdmitUnlessAssessing_AVisitNoWorkerOwnsGoesWithItsJob|TestFail_OnAStalledCompleteJobAtFetching_ResumesItForAssessing|TestFail_OnAUserPausedJob_DefersWithoutResumingIt|TestAdmitUnlessAssessing_KeepsNoEmptyReason|TestFinalizeRegistered_AnEmptyReasonAtAssessing_IsNoFailure|TestHopelessCallbacks_CarryTheirTrigger)$
timeout 5m

# A deferring hand-off (Fail, the hopeless callbacks) does not admit a job at
# Assessing. Its reason waits for the job's Assessing worker, which hands the
# job over in place of its verdict, or after its report for a reason that
# arrived after it last looked.

[a hand-off by ID admits a job at Assessing]
file internal/app/postproc_admission.go
run ^TestAdmitUnlessAssessing_DefersToTheVisit$
--- anchor
	if _, admitted := a.jobs[j]; !admitted && awaitsAssessing(j) {
--- replace
	if false {
--- end

[the hopeless callbacks' hand-over does not defer]
file internal/app/app.go
run ^TestHopelessCallbacks_CarryTheirTrigger$
--- anchor
	app.finalizeRegistered(j, hopelessReason(j, trigger), true)
--- replace
	app.finalizeRegistered(j, hopelessReason(j, trigger), false)
--- end

[Fail's hand-over does not defer]
file internal/app/durability.go
--- anchor
	app.finalizeRegistered(j, reason, true)
--- replace
	app.finalizeRegistered(j, reason, false)
--- end

[enqueuePostProc ignores the deferral]
file internal/app/app.go
run ^TestFail_WithAssessingPending_DefersToTheWorkerTheTickLaunches$
--- anchor
	if deferAtAssessing {
--- replace
	if false {
--- end

[a job at Assessing is not deferred to]
file internal/app/postproc_admission.go
run ^TestAwaitsAssessing$
--- anchor
	case s.State.State == job.Assessing:
--- replace
	case s.State.State == job.Assessing && false:
--- end

[a job with Assessing pending is not deferred to]
file internal/app/postproc_admission.go
run ^TestAwaitsAssessing$
--- anchor
	case s.State.Next == job.Assessing:
--- replace
	case s.State.Next == job.Assessing && false:
--- end

[a complete job at Fetching is not deferred to]
file internal/app/postproc_admission.go
run ^TestFail_OnACompleteJobAtFetching_DefersToAssessing$
--- anchor
		return j.IsComplete()
--- replace
		return false
--- end

[a settled job is deferred to]
file internal/app/postproc_admission.go
run ^TestAwaitsAssessing$
--- anchor
	if !s.IsOpen() {
--- replace
	if false {
--- end

[a job past its verdict is deferred to]
file internal/app/postproc_admission.go
run ^TestAwaitsAssessing$
--- anchor
	case s.State.Next != job.StateUnset:
--- replace
	case s.State.Next != job.StateUnset && false:
--- end

[the Assessing worker reports its verdict over a deferred reason]
file internal/app/runner.go
run ^TestFail_WithAssessingPending_DefersToTheWorkerTheTickLaunches$
--- anchor
	if reasons := r.app.postProcAdmissions.takeDeferred(j); len(reasons) > 0 {
--- replace
	if reasons := []string(nil); len(reasons) > 0 {
--- end

[a reason deferred after the worker looked is never handed over]
file internal/app/runner.go
run ^TestFail_BetweenTheVerdictAndItsReport_IsHandedOffAfterTheReport$
--- anchor
	for _, reason := range r.app.postProcAdmissions.endAssess(j, visit) {
--- replace
	for _, reason := range []string(nil) {
--- end

[the Assessing worker begins no visit]
file internal/app/runner.go
run ^TestFinalizeRegistered_AnEmptyReasonAtAssessing_IsNoFailure$
--- anchor
	visit := r.app.postProcAdmissions.beginAssess(j)
--- replace
	visit := (*assessVisit)(nil)
--- end

[an earlier visit's end takes a later visit's reasons]
file internal/app/postproc_admission.go
run ^TestBeginAssess_AnEarlierVisitsEndTakesNothingDeferredToALaterOne$
--- anchor
	if a.assessing[key] != visit {
--- replace
	if false {
--- end

[a visit no worker owns outlives its job]
file internal/app/postproc_admission.go
run ^TestAdmitUnlessAssessing_AVisitNoWorkerOwnsGoesWithItsJob$
--- anchor
	defer a.mu.Unlock()
	delete(a.assessing, key)
--- replace
	defer a.mu.Unlock()
	_ = key
--- end

[a new visit adopts one still owned]
file internal/app/postproc_admission.go
run ^TestBeginAssess_AnEarlierVisitsEndTakesNothingDeferredToALaterOne$
--- anchor
	case v.owned:
--- replace
	case false:
--- end

# A job Stall paused, which Fail then defers, is resumed by the hand-over
# (handOverStall) so the tick launches the Assessing worker; a pause the user
# made is not.

[a deferred Fail leaves Stall's pause in place]
file internal/app/stall.go
run ^TestFail_OnAStalledCompleteJobAtFetching_ResumesItForAssessing$
--- anchor
	if !app.clearStall(j.ID()) || !deferred {
--- replace
	if !app.clearStall(j.ID()) || true {
--- end

[a deferred Fail resumes a pause the user made]
file internal/app/stall.go
run ^TestFail_OnAUserPausedJob_DefersWithoutResumingIt$
--- anchor
	if !app.clearStall(j.ID()) || !deferred {
--- replace
	if _ = app.clearStall(j.ID()); !deferred {
--- end

[a stall record Stall did not park counts as its pause]
file internal/app/stall.go
run ^TestFail_OnAUserPausedJob_DefersWithoutResumingIt$
--- anchor
	delete(app.stalls, jobID)
	return ok && rec.parked
--- replace
	delete(app.stalls, jobID)
	return ok && (rec.parked || true)
--- end

[a deferred hand-over is settled as an admitted one]
file internal/app/app.go
run ^TestFail_OnAStalledCompleteJobAtFetching_ResumesItForAssessing$
--- anchor
		app.handOverStall(j, true)
--- replace
		app.handOverStall(j, false)
--- end

# "" is no reason, as admitLocked reads it: deferring it would have the
# Assessing worker settle a healthy job Failed.

[an empty reason is deferred to the Assessing worker]
file internal/app/postproc_admission.go
run ^TestAdmitUnlessAssessing_KeepsNoEmptyReason$
--- anchor
		if failMsg == "" {
--- replace
		if false {
--- end
