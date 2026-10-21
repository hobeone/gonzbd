pkg ./internal/dispatch/
run TestResumeJobByUser_|TestResume_LeavesAFilingOwed

[the user's resume does not approve a blocked job]
file internal/dispatch/registry.go
--- anchor
		e.h.Unwanted = unwanted.StateApproved
	}
	if d.resumeDecidedHook != nil {
--- replace
		_ = e
	}
	if d.resumeDecidedHook != nil {
--- end

[the user's resume approves every job, blocked or not]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted == unwanted.StateBlocked {
		if !byUser {
--- replace
	if true {
		if !byUser {
--- end

[a plain resume unblocks a blocked job]
file internal/dispatch/registry.go
--- anchor
		if !byUser {
--- replace
		if false && !byUser {
--- end

[a resume approves, or refuses, a job owed its filing (#1033)]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted == unwanted.StateBlocked {
		if !byUser {
--- replace
	if e.h.Unwanted.Blocked() {
		if !byUser {
--- end
