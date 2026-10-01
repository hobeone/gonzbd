pkg ./internal/app/
run ^TestRemoveJob_CompletionAfterTheAbort_StartsNoUnpacker$

# No DirectUnpacker is started for a job instance RemoveJob has marked
# removed, so a completion landing after RemoveJob's duOrch.abortJob leaves
# no unpacker that nothing aborts.

[maybeStart starts an unpacker for a removed job]
file internal/app/directunpack_orchestrator.go
--- anchor
		if app.postProcAdmissions.has(j) || app.transitions.wasRemoved(j) {
--- replace
		if app.postProcAdmissions.has(j) {
--- end

[RemoveJob marks the job removed only as it returns, after the abort]
file internal/app/app.go
--- anchor
	app.transitions.markRemoved(j)
	name := j.Name()
--- replace
	name := j.Name()
	defer app.transitions.markRemoved(j)
--- end
