pkg ./internal/dispatch/
run TestCancelFor_LeavesALaterInstanceAlone$|TestCancel_(NoJobReturnsError|LatchesAndKicksForARegisteredJob)$

# CancelFor's instance check, and the latch it guards, each removed on its own.

[a removed instance still cancels the later one]
file internal/dispatch/dispatch.go
--- anchor
	if !ok || (expected != nil && e.j != expected) {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: Cancel: no job %q: %w", id, ErrNotFound)
--- replace
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("dispatch: Cancel: no job %q: %w", id, ErrNotFound)
--- end

[the matched instance is not latched]
file internal/dispatch/dispatch.go
--- anchor
	if err := d.q.Cancel(j); err != nil {
		return fmt.Errorf("dispatch: Cancel(%s): %w", id, err)
--- replace
	if _, err := j, error(nil); err != nil {
		return fmt.Errorf("dispatch: Cancel(%s): %w", id, err)
--- end
