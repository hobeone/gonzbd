pkg ./internal/dispatch/
run ^(TestPauseInstance_LeavesALaterInstanceAlone|TestResumeInstance_LeavesALaterInstanceAlone|TestSetFailReason_LeavesALaterInstanceAlone)$

# The instance doors Application.Stall and Application.Fail use (#914): each
# acts only on the instance registered under its ID.

[PauseInstance pauses whatever instance holds the ID]
file internal/dispatch/registry.go
--- anchor
	return d.pauseFor(j.ID(), j)
--- replace
	return d.pauseFor(j.ID(), nil)
--- end

[ResumeInstance resumes whatever instance holds the ID]
file internal/dispatch/registry.go
--- anchor
	return d.resume(j.ID(), j, false)
--- replace
	return d.resume(j.ID(), nil, false)
--- end

[resume ignores the expected instance]
file internal/dispatch/registry.go
--- anchor
	if !ok || (expected != nil && e.j != expected) {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: resume %s: %w", id, ErrNotFound)
--- replace
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: resume %s: %w", id, ErrNotFound)
--- end

[SetFailReason writes whatever instance holds the ID]
file internal/dispatch/registry.go
--- anchor
	if !ok || e.j != j {
		return fmt.Errorf("dispatch: set fail reason %s: %w", id, ErrNotFound)
--- replace
	if !ok {
		return fmt.Errorf("dispatch: set fail reason %s: %w", id, ErrNotFound)
--- end
