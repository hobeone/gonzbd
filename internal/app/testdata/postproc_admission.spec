pkg ./internal/app/
run Test(Fail_WhilePostProcessing_IsNotedAndLeavesTheStatusToTheStages|RunPostProc_DuringTheFinalizerTail_EnqueuesNoSecondCopy|Enqueue_DuringTheDirectUnpackWait_EnqueuesNoSecondCopy|RemoveJob_EndsThePostProcessingAdmission|JobFinalizerCancelled_EndsTheAdmissionWithoutADispatcher|PostProcAdmissions_AdmitsEachInstanceOnce|PostProcAdmissions_KeepsTheFirstFailureReason|WithFailureNotes_LeavesThePostProcessorsJobAlone)$

# enqueuePostProc admitting at most one post-processing run of a job instance
# at a time; a refused call's failure reason reaching the run only before the
# seal, and otherwise noted without changing the history status.

[enqueuePostProc ignores a refused admission]
file internal/app/app.go
--- anchor
	switch admit(j, failMsg) {
--- replace
	switch _ = admit(j, failMsg); admitted {
--- end

[finalize ends the admission before its tail rather than after it]
file internal/app/job_finalizer.go
--- anchor
	defer app.postProcAdmissions.release(ppJob.Job)
	if app.finalizeHook != nil {
--- replace
	app.postProcAdmissions.release(ppJob.Job)
	if app.finalizeHook != nil {
--- end

[finalize never ends the admission]
file internal/app/job_finalizer.go
--- anchor
	defer app.postProcAdmissions.release(ppJob.Job)
	if app.finalizeHook != nil {
--- replace
	if app.finalizeHook != nil {
--- end

[finalize drops the noted reasons]
file internal/app/job_finalizer.go
--- anchor
	notes := app.postProcAdmissions.notes(ppJob.Job)
--- replace
	notes := []string(nil)
	_ = app.postProcAdmissions.notes(ppJob.Job)
--- end

[a late reason overrides the status the stages decided]
file internal/app/history_helper.go
--- anchor
	cp := *ppJob
--- replace
	cp := *ppJob
	cp.FailMsg = notes[0]
--- end

[the notes are written onto the post-processor's own job]
file internal/app/history_helper.go
--- anchor
	cp := *ppJob
	cp.StageLog = append(slices.Clone(ppJob.StageLog), postproc.StageLogEntry{
--- replace
	cp := *ppJob
	ppJob.StageLog = append(ppJob.StageLog, postproc.StageLogEntry{})
	cp.StageLog = append(slices.Clone(ppJob.StageLog[:len(ppJob.StageLog)-1]), postproc.StageLogEntry{
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
		admittedFailMsg, handOver, ok := app.postProcAdmissions.beginHandOver(j, &app.transitions)
--- replace
		admittedMsg, handOver, ok := app.postProcAdmissions.beginHandOver(j, &app.transitions)
		admittedFailMsg := failMsg + admittedMsg[:0]
--- end

[the enqueue does not seal, so a late reason becomes the run's]
file internal/app/postproc_admission.go
--- anchor
	cur.sealed = true
	cur.busy = make(chan struct{})
--- replace
	cur.busy = make(chan struct{})
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

[a refused call's reason never becomes the run's]
file internal/app/postproc_admission.go
--- anchor
		if cur.failMsg == "" && !cur.sealed {
--- replace
		if false {
--- end

[a repeated reason is noted again]
file internal/app/postproc_admission.go
--- anchor
		if failMsg == "" || failMsg == cur.failMsg {
--- replace
		if failMsg == "" {
--- end

[a repeated note is noted again]
file internal/app/postproc_admission.go
--- anchor
		if slices.Contains(cur.notes, failMsg) {
--- replace
		if false && slices.Contains(cur.notes, failMsg) {
--- end
