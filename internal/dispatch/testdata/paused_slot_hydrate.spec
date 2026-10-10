pkg ./internal/dispatch/
run TestReconcileResidency_DoesNotRehydrateAPausedSlotHolder

# A paused slot holder is not hydrated: a residency fault that parked it would
# otherwise re-read and re-fault it on every tick.

[the hydrate arm ignores the pause]
file internal/dispatch/tick.go
--- anchor
	case v.Holds && v.Intent != job.IntentPause && !d.isResident(j.ID()):
--- replace
	case v.Holds && !d.isResident(j.ID()):
--- end
