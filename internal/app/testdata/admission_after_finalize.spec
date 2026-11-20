pkg ./internal/app/
run TestFail_WhilePostProcessing_IsNotedAndLeavesTheStatusToTheStages

[the admission's release dropped from finalize]
file internal/app/job_finalizer.go
--- anchor
func (f *jobFinalizer) finalize(ppJob *postproc.Job) {
	app := f.app
	defer app.postProcAdmissions.release(ppJob.Job)
--- replace
func (f *jobFinalizer) finalize(ppJob *postproc.Job) {
	app := f.app
--- end
