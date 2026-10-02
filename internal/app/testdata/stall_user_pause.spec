pkg ./internal/app/
run TestReevaluateStall_KeepsAUserPauseStallWasCalledOn|TestStall_OnAnUnparkedRecordOfAUserPausedJob|TestStall_ASecondFaultKeepsTheParkItOwns|TestReevaluateStall_ReleasesItsParkOnceItResumes|TestStallLost_ClaimsNoPause

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

[a resume leaves its claim on the pause behind]
file internal/app/stall.go
--- anchor
		app.releasePark(jobID)
--- replace
		_ = jobID
--- end

[stallLost claims a pause it never made]
file internal/app/stall.go
--- anchor
	app.setStallReasonLocked(jobID, reason, false)
--- replace
	app.setStallReasonLocked(jobID, reason, true)
--- end
