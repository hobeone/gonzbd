pkg ./internal/app/
run ^TestPauseJob_FetchResultsLandingAfterThePauseAreRecorded$

# A paused Fetching job gives back its lease but keeps its manifest, so a
# barrier's ack and an assembler rejection that land after the pause are
# recorded with the job's counters.

[a paused job that gave back its lease is evicted]
file internal/dispatch/tick.go
--- anchor
	case !v.Holds && v.Intent != job.IntentPause && d.isResident(j.ID()):
--- replace
	case !v.Holds && d.isResident(j.ID()):
--- end
