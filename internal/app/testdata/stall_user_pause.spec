pkg ./internal/app/
run TestReevaluateStall_KeepsAUserPauseStallWasCalledOn|TestStall_OnAnUnparkedRecordOfAUserPausedJob|TestStall_ASecondFaultKeepsTheParkItOwns

[Stall claims a pause the user already set]
file internal/app/durability.go
--- anchor
		if j, ok := app.dispatcher.Job(jobID); ok && j.Intent() == job.IntentPause {
--- replace
		if j, ok := app.dispatcher.Job(jobID); ok && j.Intent() == job.IntentPause && false {
--- end

[a second fault releases the claim on a pause Stall made]
file internal/app/stall.go
--- anchor
	if claimPause {
		rec.parked = true
	}
--- replace
	rec.parked = claimPause
--- end
