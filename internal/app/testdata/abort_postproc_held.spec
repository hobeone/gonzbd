pkg ./internal/app/
run TestRemoveJob_(WaitsForTheCancelledStageToStop|ReleasesARunningPostProcessingJob|ReleasesARepairingJobPostProcessingDoesNotHold)$

# appWorkers.Abort leaving the launch claim of a job the post-processor holds
# to post-processing's own release, each side on its own: the claim must not be
# released while the post-processor still runs the job, and must still be
# released when it does not hold the job at all.

[the abort yields a job the post-processor is still running]
file internal/app/dispatcher_wiring.go
--- anchor
	if pp != nil && pp.Has(jobID) {
--- replace
	if pp != nil && false {
--- end

[the abort decides on the job's state instead of the post-processor holding it]
file internal/app/dispatcher_wiring.go
--- anchor
	if pp != nil && pp.Has(jobID) {
--- replace
	if pp != nil && j.Snapshot().State.State == job.Repairing {
--- end

[the abort never yields]
file internal/app/dispatcher_wiring.go
--- anchor
	if pp != nil && pp.Has(jobID) {
--- replace
	if pp != nil {
--- end
