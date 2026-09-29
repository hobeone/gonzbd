pkg ./internal/app/
run TestRemoveJob_(WaitsForTheCancelledStageToStop|ReleasesARunningPostProcessingJob|ReleasesARepairingJobPostProcessingDoesNotHold|CancelsAJobThatReachesPostProcessingBetweenItsCancels)$

# appWorkers.Abort leaving the launch claim of a job the post-processor holds
# to post-processing's own release, each side on its own: the claim must not be
# released while the post-processor still runs the job, and must still be
# released when it does not hold the job at all. Then RemoveJob's dispatcher
# cancel preceding its post-processing cancel, which that depends on.
#
# The finalizer's release of a Repairing job that finished after the abort
# left its claim (TestRemoveJob_ReleasesAJobPostProcessingFinishesAfterTheAbort)
# has no mutation here: dispatcher.Remove cancels the job again, and that
# abort yields it once post-processing has let it go, so removing the
# finalizer's release alone leaves the test green. finalizer_release.spec
# mutates that release against a test where nothing else releases the claim.

[the abort yields a job the post-processor is still running]
file internal/app/dispatcher_wiring.go
--- anchor
	if pp != nil && pp.HasJob(j) {
--- replace
	if pp != nil && false {
--- end

[the abort decides on the job's state instead of the post-processor holding it]
file internal/app/dispatcher_wiring.go
--- anchor
	if pp != nil && pp.HasJob(j) {
--- replace
	if pp != nil && j.Snapshot().State.State == job.Repairing {
--- end

[the abort never yields]
file internal/app/dispatcher_wiring.go
--- anchor
	if pp != nil && pp.HasJob(j) {
--- replace
	if pp != nil {
--- end

[RemoveJob cancels post-processing before the dispatcher again]
file internal/app/app.go
--- anchor
	_ = app.dispatcher.Cancel(id)
	if app.removeCancelGapHook != nil {
		app.removeCancelGapHook(id)
	}
	app.postProcessor.Cancel(id)
--- replace
	app.postProcessor.Cancel(id)
	if app.removeCancelGapHook != nil {
		app.removeCancelGapHook(id)
	}
	_ = app.dispatcher.Cancel(id)
--- end
