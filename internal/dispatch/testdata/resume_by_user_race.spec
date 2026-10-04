# Red check for the user's resume racing BlockUnwanted
# (internal/dispatch/registry.go resume): the intent must be set in the same
# d.mu span as the decision, or a Blocked job is left running.
#
#     go run ./scripts/mutate internal/dispatch/testdata/resume_by_user_race.spec
pkg ./internal/dispatch/
run TestResumeJobByUser_Racing

[the user's resume lets go of d.mu between deciding and setting the intent]
file internal/dispatch/registry.go
--- anchor
		d.resumeDecidedHook()
--- replace
		d.mu.Unlock()
		d.resumeDecidedHook()
		d.mu.Lock()
--- end
