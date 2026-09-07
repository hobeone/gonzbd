pkg ./internal/dispatch/
run TestYieldedFor_JobMismatch_NoOpsAndPreservesNewAttempt

[neutering pointer identity check in YieldedFor]
file internal/dispatch/worker.go
--- anchor
	if !ok || (expected != nil && e.j != expected) {
--- replace
	if !ok {
--- end
