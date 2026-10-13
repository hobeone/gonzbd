pkg ./internal/app/
run ^(TestFinalizeHopeless_ClearsTheStallOfTheJobItHandsOver|TestFinalizeHopeless_OnAStalledJobAwaitingAssessing_ResumesItForAssessing)$

# A job leaves the stalled list when it is handed to post-processing, on the
# admission and on a deferral to its Assessing worker, whichever door handed it
# over (enqueuePostProc's handOverStall), not at the next re-evaluation.

[the admission keeps the stall record]
file internal/app/app.go
run ^TestFinalizeHopeless_ClearsTheStallOfTheJobItHandsOver$
--- anchor
		app.handOverStall(j, false)
--- replace
		_ = j
--- end

[the deferral keeps the stall record and Stall's pause]
file internal/app/app.go
run ^TestFinalizeHopeless_OnAStalledJobAwaitingAssessing_ResumesItForAssessing$
--- anchor
		app.handOverStall(j, true)
--- replace
		_ = j
--- end

[the hand-over resumes Stall's pause but keeps its record]
file internal/app/stall.go
--- anchor
	if !app.clearStall(j.ID()) || !deferred {
--- replace
	if !app.weParked(j.ID()) || !deferred {
--- end
