# Red check for Dispatcher.BlockUnwanted (internal/dispatch/registry.go): each
# mutation neuters one decision and a named test must die.
#
#     go run ./scripts/mutate internal/dispatch/testdata/block_unwanted.spec
pkg ./internal/dispatch/
run TestBlockUnwanted_|TestResumeJob_Racing|TestResumeJobByUser_Racing
timeout 5m

[a blocked or approved job is moved again]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted != unwanted.StateNone {
--- replace
	if false {
--- end

[the move records approved, not blocked]
file internal/dispatch/registry.go
--- anchor
	e.h.Unwanted = unwanted.StateBlocked
--- replace
	e.h.Unwanted = unwanted.StateApproved
--- end

[the pause request is ignored]
file internal/dispatch/registry.go
--- anchor
	if pause {
--- replace
	if false {
--- end

[ResumeJob lets go of d.mu between deciding and setting the intent]
file internal/dispatch/registry.go
--- anchor
		d.resumeDecidedHook()
--- replace
		d.mu.Unlock()
		d.resumeDecidedHook()
		d.mu.Lock()
--- end

[a stale instance blocks the job registered under its ID]
file internal/dispatch/registry.go
--- anchor
	if !ok || e.j != j {
--- replace
	if !ok {
--- end

[a block without the pause request pauses anyway]
file internal/dispatch/registry.go
--- anchor
	if pause {
--- replace
	if true {
--- end
