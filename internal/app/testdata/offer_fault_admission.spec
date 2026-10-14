pkg ./internal/app/
run TestOfferFaultToAdmission

# offerFaultToAdmission logs one Error line per admitOutcome, with the fault's
# source, and offers the fault's own reason to the admission.

[a kept reason logs the noted arm's message]
file internal/app/app.go
--- anchor
		msg = "storage fault after the hand-over to post-processing; the run fails with it"
--- replace
		msg = "storage fault after the hand-over to post-processing; the run already has a failure reason or is sealed, so this one is noted"
--- end

[an ended admission falls into the unreachable arm]
file internal/app/app.go
--- anchor
	case refusedEnded:
		msg = "storage fault after the job's post-processing ended; it has no run left to reach and is logged only"
--- replace
	case admitOutcome(-1):
		msg = "storage fault after the job's post-processing ended; it has no run left to reach and is logged only"
--- end

[a closed-notes admission reads as a repeat]
file internal/app/app.go
--- anchor
	case refusedNotesClosed:
		msg = "storage fault after the hand-over to post-processing; the run's history entry is already being written, so the fault is logged only"
--- replace
	case admitOutcome(-1):
		msg = "storage fault after the hand-over to post-processing; the run's history entry is already being written, so the fault is logged only"
--- end

[the source attribute is dropped]
file internal/app/app.go
--- anchor
	app.log.Error(msg, "job", j.ID(), "source", source, "fault", f.Error())
--- replace
	app.log.Error(msg, "job", j.ID(), "source", "", "fault", f.Error())
--- end

[the log line is demoted below Error]
file internal/app/app.go
--- anchor
	app.log.Error(msg, "job", j.ID(), "source", source, "fault", f.Error())
--- replace
	app.log.Warn(msg, "job", j.ID(), "source", source, "fault", f.Error())
--- end

[the admission is offered no reason]
file internal/app/app.go
--- anchor
	switch outcome := app.postProcAdmissions.admit(j, faultReason(f)); outcome {
--- replace
	switch outcome := app.postProcAdmissions.admit(j, ""); outcome {
--- end
