pkg ./internal/dispatch/
run ^(TestLaunch_ReportBeforeClaimLeavesNoStrandedClaim|TestLaunch_DeclinedLaunchReturnsTheGrant)$

# The re-check has two conjuncts. The report case makes Running false; a
# cancel or pause landing before the claim leaves Running true and changes
# only the intent, so the intent conjunct is mutated on its own.

[launch trusts the Running check it made before the claim]
file internal/dispatch/worker.go
--- anchor
	v = d.q.Render(j)
	if !v.Running || v.Intent != job.IntentRun {
--- replace
	v = d.q.Render(j)
	if false {
--- end

[the post-claim re-check ignores a cancel or pause latched before the claim]
file internal/dispatch/worker.go
--- anchor
	v = d.q.Render(j)
	if !v.Running || v.Intent != job.IntentRun {
--- replace
	v = d.q.Render(j)
	if !v.Running {
--- end

[the failed re-check returns without releasing the claim it took]
file internal/dispatch/worker.go
--- anchor
		d.clearLaunched(j.ID())
		return
	}
	d.mu.Lock()
--- replace
		return
	}
	d.mu.Lock()
--- end
