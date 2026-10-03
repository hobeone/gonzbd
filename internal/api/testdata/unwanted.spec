pkg ./internal/api/
run TestQueueResume_ApprovesAnUnwantedBlockedJob|TestQueueSlot_UnwantedLabel|TestHistorySlot_UnwantedExt|TestHistoryRetry_AllowUnwanted|TestHistoryRetry_UnwantedRefusalIsAConflict

[the user's resume does not approve]
file internal/api/queue.go
--- anchor
				_ = s.dispatcher.ResumeJobByUser(id)
--- replace
				_ = s.dispatcher.ResumeJob(id)
--- end

[no label for a flagged job]
file internal/api/queue.go
--- anchor
	if h.Unwanted != unwanted.StateNone {
--- replace
	if false && h.Unwanted != unwanted.StateNone {
--- end

[an approved job loses its label]
file internal/api/queue.go
--- anchor
	if h.Unwanted != unwanted.StateNone {
--- replace
	if h.Unwanted == unwanted.StateBlocked {
--- end

[labels sent as null]
file internal/api/queue.go
--- anchor
	return []string{}
}
--- replace
	return nil
}
--- end

[history drops the state]
file internal/api/history.go
--- anchor
			UnwantedExt:  e.Unwanted,
--- replace
			UnwantedExt:  0,
--- end

[allow_unwanted is ignored]
file internal/api/history.go
--- anchor
	if formValue(r, "allow_unwanted") == "1" {
--- replace
	if false && formValue(r, "allow_unwanted") == "1" {
--- end

[a refusal is a server fault]
file internal/api/history.go
--- anchor
		if errors.Is(err, app.ErrUnwantedRefused) {
--- replace
		if false && errors.Is(err, app.ErrUnwantedRefused) {
--- end
