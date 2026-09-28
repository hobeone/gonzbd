pkg ./internal/dispatch/
run TestCancelJob_(LeavesALaterInstanceAlone|NilJobReturnsErrNotFound)$|TestCancel_(NoJobReturnsError|LatchesAndKicksForARegisteredJob)$|TestYieldedFor_JobMismatch_NoOpsAndPreservesNewAttempt$

# lookupFor's instance check, which CancelJob and YieldedFor both rely on, and
# the latch CancelJob guards, each removed on its own.

[a removed instance still matches the later one]
file internal/dispatch/dispatch.go
--- anchor
	if !ok || (expected != nil && e.j != expected) {
--- replace
	if !ok {
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
