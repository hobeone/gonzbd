pkg ./internal/dispatch/
run TestResumeJobByUser_

[the user's resume does not approve a blocked job]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted == unwanted.StateBlocked {
--- replace
	if false && e.h.Unwanted == unwanted.StateBlocked {
--- end

[the user's resume approves every job, blocked or not]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted == unwanted.StateBlocked {
--- replace
	if true {
--- end
