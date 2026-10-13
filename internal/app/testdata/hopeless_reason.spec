pkg ./internal/app/
run ^(TestHopelessReason|TestHopelessCallbacks_CarryTheirTrigger)$

# hopelessReason is the one producer of a hopeless job's reason (#916), and
# each detector's callback names its own trigger.

[failMsgForJob's verdict no longer comes first]
file internal/app/app.go
--- anchor
	if msg := failMsgForJob(j); msg != "" {
--- replace
	if msg := failMsgForJob(j); false && msg != "" {
--- end

[the trigger is ignored: every fallback is the early-abort wording]
file internal/app/app.go
--- anchor
	if trigger == hopelessEarlyAbort {
--- replace
	if true {
--- end

[the pipeline's callback names the health gate]
file internal/app/app.go
--- anchor
			app.finalizeHopeless(jobID, hopelessEarlyAbort)
--- replace
			app.finalizeHopeless(jobID, hopelessHealthGate)
--- end

[the downloader's callback names the early abort]
file internal/app/app.go
--- anchor
			app.finalizeHopeless(jobID, hopelessHealthGate)
--- replace
			app.finalizeHopeless(jobID, hopelessEarlyAbort)
--- end
