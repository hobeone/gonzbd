pkg ./internal/app/
run Test(Fail_AJobInPostProcessingIsNotDispatched|PostProcAdmissions_HasIsPerInstance)$

# A job admitted to post-processing is not dispatched, although a job handed
# over from Fetching keeps a dispatchable row until the finalizer cancels it.
#
# This used to also carry a third mutation neutering buildDispatchPlan's own
# `d.handedOff(j)` check (dispatch.go:88, the queue-time gate). Once
# fetchArticle grew its own HandedOff check, TestFail_...IsNotDispatched
# stopped discriminating that mutation — the request is queued as before but
# now dropped one layer later, before any network I/O, so the test's "was it
# fetched" assertion still passes. The queue-time gate is pinned on its own
# terms, where removing it changes what buildDispatchPlan reports directly:
# internal/downloader/testdata/handed_off_gate.spec.

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
