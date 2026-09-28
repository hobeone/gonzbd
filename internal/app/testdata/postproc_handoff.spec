pkg ./internal/app/
run Test(Fail_AJobInPostProcessingIsNotDispatched|PostProcAdmissions_HasIsPerInstance)$

# A job admitted to post-processing is not dispatched, although a job handed
# over from Fetching keeps a dispatchable row until the finalizer cancels it.

[the downloader is not told which jobs are handed off]
file internal/app/app.go
--- anchor
		HandedOff: app.postProcAdmissions.has,
--- replace
		HandedOff: nil,
--- end

[has never reports an admission]
file internal/app/postproc_admission.go
--- anchor
	_, ok := a.jobs[j]
	return ok
--- replace
	_ = a.jobs[j]
	return false
--- end

[buildDispatchPlan ignores the hand-off]
file internal/downloader/dispatch.go
--- anchor
		if !ok || !j.Resident() || d.handedOff(j) {
--- replace
		if !ok || !j.Resident() {
--- end
