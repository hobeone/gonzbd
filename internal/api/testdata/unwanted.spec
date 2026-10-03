pkg ./internal/api/
run TestQueueResume_ApprovesAnUnwantedBlockedJob|TestQueueSlot_UnwantedLabel|TestHistorySlot_UnwantedExt|TestHistoryRetry_AllowUnwanted|TestHistoryRetry_UnwantedRefusalIsAConflict|TestQueueResume_NZBKeyCannotApprove|TestHistoryRetry_NZBKeyCannotAllowUnwanted

[the user's resume does not approve]
file internal/api/queue.go
--- anchor
				_ = s.dispatcher.ResumeJobByUser(id)
--- replace
				_ = s.dispatcher.ResumeJob(id)
--- end

[the NZB key may approve]
file internal/api/middleware.go
--- anchor
	return callerLevel(r, s.getAuth()) >= LevelAdmin
--- replace
	return callerLevel(r, s.getAuth()) >= LevelProtected
--- end

[a blocked job is resumed without the full key]
file internal/api/queue.go
--- anchor
			if row, ok := s.dispatcher.Row(id); ok && row.Header.Unwanted == unwanted.StateBlocked {
--- replace
			if row, ok := s.dispatcher.Row(id); false && ok && row.Header.Unwanted == unwanted.StateBlocked {
--- end

[retry allow_unwanted with the NZB key]
file internal/api/history.go
--- anchor
		if !s.canApproveUnwanted(r) {
--- replace
		if false && !s.canApproveUnwanted(r) {
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
