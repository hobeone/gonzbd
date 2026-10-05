# Red check for the resume sweep's residency release (internal/app/resume_startup.go):
# each mutation neuters one decision and a named test must die.
#
#     go run ./scripts/mutate internal/app/testdata/resume_residency.spec
pkg ./internal/app/
run TestResumeJob_AnAbortedIterationStillReleasesWhatItHydrated|TestResumeAllJobs_EvictsAJobItHydrated|TestResumeAllJobs_EvictedJobIsHydratedAgainWhenItHolds|TestResumeAllJobs_KeepsAJobAlreadyResident|TestResumeAllJobs_KeepsAHydratedJobThatHoldsALease|TestResumeAllJobs_KeepsAHydratedJobAdmittedToPostProcessing
timeout 10m

[the sweep never releases what it hydrated]
file internal/app/resume_startup.go
--- anchor
	if !ok || row.View.Holds || app.postProcAdmissions.has(j) {
		return
	}
	app.residency.Evict(jobID)
--- replace
	_, _ = row, ok
--- end

[the release ignores whether the job holds what its position needs]
file internal/app/resume_startup.go
--- anchor
	if !ok || row.View.Holds || app.postProcAdmissions.has(j) {
--- replace
	if !ok || (row.View.Holds && false) || app.postProcAdmissions.has(j) {
--- end

[the release ignores a job admitted to post-processing]
file internal/app/resume_startup.go
--- anchor
	if !ok || row.View.Holds || app.postProcAdmissions.has(j) {
--- replace
	if !ok || row.View.Holds {
--- end


[the release does not check the job was not resident before the sweep]
file internal/app/resume_startup.go
--- anchor
	if app.residency != nil && !j.Resident() {
--- replace
	if app.residency != nil {
--- end
