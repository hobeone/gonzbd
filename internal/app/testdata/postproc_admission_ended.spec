pkg ./internal/app/
run Test(Enqueue_AfterAFinalizeThatCouldNotRemoveTheJob_AdmitsNoSecondRun|Enqueue_AfterACancelledRun_AdmitsNoSecondRun|PostProcAdmissions_EndedForgetsACollectedJob)$

# An instance whose post-processing admission has ended is not admitted again,
# with the record's two halves, admit's refusal and release's record, each
# removed on its own, and the record's cleanup neutered.

[admit ignores an ended admission]
file internal/app/postproc_admission.go
--- anchor
	if _, ok := a.ended[weak.Make(j)]; ok {
--- replace
	if false {
--- end

[release records no ended admission]
file internal/app/postproc_admission.go
--- anchor
	a.ended[key] = runtime.AddCleanup(j, a.forgetEnded, key)
--- replace
	_ = key
--- end

[the record of a collected job is never dropped]
file internal/app/postproc_admission.go
--- anchor
	delete(a.ended, key)
--- replace
	_ = key
--- end
