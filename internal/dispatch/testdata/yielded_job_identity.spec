pkg ./internal/dispatch/
run TestYieldedFor_JobMismatch_NoOpsAndPreservesNewAttempt

[neutering the pointer identity check YieldedFor relies on (lookupFor)]
file internal/dispatch/dispatch.go
--- anchor
	if !ok || (expected != nil && e.j != expected) {
--- replace
	if !ok {
--- end
