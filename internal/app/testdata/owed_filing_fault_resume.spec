# Red check for #1033: a job the fail action blocked is filed after a restart
# whose hydration hit a storage fault, and the user's resume of the parked job
# does not approve it. Each half of the fix is reverted on its own.
#
#     go run ./scripts/mutate internal/app/testdata/owed_filing_fault_resume.spec

pkg ./internal/app/
run ^TestFileOwedUnwantedFailures_StorageFaultThenUserResumeStillFiles$
timeout 5m

[the owed filing is read from the intent again, so the fault's pause drops it]
file internal/app/unwanted.go
--- anchor
	if !ok || row.Header.Unwanted != unwanted.StateFailOwed {
--- replace
	if !ok || !row.Header.Unwanted.Blocked() || j.Intent() != job.IntentRun {
--- end

[the fail action's move records a block the user's resume approves]
file internal/dispatch/registry.go
--- anchor
	e.h.Unwanted = unwanted.StateFailOwed
--- replace
	e.h.Unwanted = unwanted.StateBlocked
--- end

[the user's resume approves a job owed its filing]
file internal/dispatch/registry.go
--- anchor
	if e.h.Unwanted == unwanted.StateBlocked {
		if !byUser {
--- replace
	if e.h.Unwanted.Blocked() {
		if !byUser {
--- end

[the history entry records the queue-only owed state]
file internal/app/history_helper.go
--- anchor
		Unwanted:     ppJob.Unwanted.ForHistory(),
--- replace
		Unwanted:     ppJob.Unwanted,
--- end
