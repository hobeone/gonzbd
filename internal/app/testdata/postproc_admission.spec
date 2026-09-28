pkg ./internal/app/
run Test(Fail_WhilePostProcessing_EnqueuesNoSecondCopy|RunPostProc_DuringTheFinalizerTail_EnqueuesNoSecondCopy|Enqueue_DuringTheDirectUnpackWait_EnqueuesNoSecondCopy|RemoveJob_EndsThePostProcessingAdmission|PostProcAdmissions_AdmitsEachInstanceOnce|PostProcAdmissions_KeepsTheFirstFailureReason)$

# enqueuePostProc admitting each job instance to post-processing once, and
# the failure reason a refused call offers reaching the admitted run.

[enqueuePostProc ignores a refused admission]
file internal/app/app.go
--- anchor
	switch app.postProcAdmissions.admit(j, failMsg) {
--- replace
	switch _ = app.postProcAdmissions.admit(j, failMsg); admitted {
--- end

[finalize ends the admission before its tail rather than after it]
file internal/app/job_finalizer.go
--- anchor
		defer app.postProcAdmissions.release(ppJob.Job)
		if msg := app.postProcAdmissions.seal(ppJob.Job); ppJob.FailMsg == "" && msg != "" {
			ppJob.FailMsg = msg
		}
--- replace
		if msg := app.postProcAdmissions.seal(ppJob.Job); ppJob.FailMsg == "" && msg != "" {
			ppJob.FailMsg = msg
		}
		app.postProcAdmissions.release(ppJob.Job)
--- end

[finalize never ends the admission]
file internal/app/job_finalizer.go
--- anchor
		defer app.postProcAdmissions.release(ppJob.Job)
		if msg := app.postProcAdmissions.seal(ppJob.Job); ppJob.FailMsg == "" && msg != "" {
--- replace
		if msg := app.postProcAdmissions.seal(ppJob.Job); ppJob.FailMsg == "" && msg != "" {
--- end

[finalize ignores a reason a refused call left]
file internal/app/job_finalizer.go
--- anchor
		if msg := app.postProcAdmissions.seal(ppJob.Job); ppJob.FailMsg == "" && msg != "" {
--- replace
		if msg := app.postProcAdmissions.seal(ppJob.Job); false && msg != "" {
--- end

[a cancelled job keeps its admission]
file internal/app/job_finalizer.go
--- anchor
	defer app.postProcAdmissions.release(ppJob.Job)
	if app.dispatcher == nil {
--- replace
	if app.dispatcher == nil {
--- end

[the post-processor is handed the reason the admitting call carried, not the admission's]
file internal/app/app.go
--- anchor
			FailMsg:              admittedFailMsg,
--- replace
			FailMsg:              failMsg + admittedFailMsg[:0],
--- end

[admissions keyed by job ID rather than instance]
file internal/app/postproc_admission.go
--- anchor
	if cur, ok := a.jobs[j]; ok {
		if failMsg == "" || failMsg == cur.failMsg {
--- replace
	var cur *postProcAdmission
	var ok bool
	for k, v := range a.jobs {
		if k.ID() == j.ID() {
			cur, ok = v, true
		}
	}
	if ok {
		if failMsg == "" || failMsg == cur.failMsg {
--- end

[a refused call's reason is never kept]
file internal/app/postproc_admission.go
--- anchor
		cur.failMsg = failMsg
		return refusedReasonKept
--- replace
		return refusedReasonKept
--- end

[a repeated reason is reported as a dropped one]
file internal/app/postproc_admission.go
--- anchor
		if failMsg == "" || failMsg == cur.failMsg {
--- replace
		if failMsg == "" {
--- end

[a reason arriving after the seal is kept]
file internal/app/postproc_admission.go
--- anchor
		if cur.failMsg != "" || cur.sealed {
--- replace
		if cur.failMsg != "" {
--- end
