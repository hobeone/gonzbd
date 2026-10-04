pkg ./internal/dispatch/
run TestResumeJobByUser_

[the user's resume does not approve a blocked job]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted == unwanted.StateBlocked {
		e.h.Unwanted = unwanted.StateApproved
--- replace
	if false && e.h.Unwanted == unwanted.StateBlocked {
		e.h.Unwanted = unwanted.StateApproved
--- end

[the user's resume approves every job, blocked or not]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted == unwanted.StateBlocked {
		e.h.Unwanted = unwanted.StateApproved
--- replace
	if true {
		e.h.Unwanted = unwanted.StateApproved
--- end

[a plain resume unblocks a blocked job]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted == unwanted.StateBlocked {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: resume %s: %w", id, ErrUnwantedBlocked)
--- replace
	if false && e.h.Unwanted == unwanted.StateBlocked {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: resume %s: %w", id, ErrUnwantedBlocked)
--- end
