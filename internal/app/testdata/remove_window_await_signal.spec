pkg ./internal/app/
run Test(RemoveJob_EndsThePostProcessingAdmission|RemoveJob_DuringTheDirectUnpackWait_PostProcessingDoesNotRun|EnqueuePostProc_RemovedBeforeItsHandOver_ReleasesTheJob|RemoveJob_InterruptsTheDirectUnpackWait|RemoveJob_ReleasesAJobPostProcessingFinishesAfterTheAbort|Fail_FromFetching_AbortsTheDirectUnpack|HandOver_AfterTheDownloadFinished_KeepsTheDirectUnpackResults|Application_IsPipelineHealthy)$

# The waits in these tests end on an event (a Wake, a hook, the enqueue
# goroutines returning, the post-processor's worker stopping) and not on a
# clock; each mutation removes one of those events or what it reports.

[the Fetching worker never wakes the downloader]
file internal/app/runner.go
--- anchor
	if w, ok := dl.(interface{ Wake() }); ok {
		w.Wake()
	}
--- replace
	_ = dl
--- end

[the worker keeps its busy marker after the job is recorded]
file internal/postproc/postproc.go
--- anchor
		p.addHistory(job)
		p.setBusyWithJob(false, nil, nil)
--- replace
		p.addHistory(job)
--- end

[RemoveJob never reaches its abort-gap hook]
file internal/app/app.go
--- anchor
	if app.removeAbortGapHook != nil {
		app.removeAbortGapHook(id)
	}
--- replace
	_ = id
--- end

[a cancelled job keeps its admission]
file internal/app/job_finalizer.go
--- anchor
	defer app.postProcAdmissions.release(ppJob.Job)
	// Cancel first, so the tick cannot relaunch the job between the yield and
--- replace
	// Cancel first, so the tick cannot relaunch the job between the yield and
--- end

[finalize never ends the admission]
file internal/app/job_finalizer.go
--- anchor
func (f *jobFinalizer) finalize(ppJob *postproc.Job) {
	app := f.app
	defer app.postProcAdmissions.release(ppJob.Job)
--- replace
func (f *jobFinalizer) finalize(ppJob *postproc.Job) {
	app := f.app
--- end
