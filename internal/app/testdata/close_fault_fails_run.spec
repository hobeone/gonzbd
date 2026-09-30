pkg ./internal/app/
run TestEnqueuePostProc_APermanentCloseFaultFailsTheRun$|TestEnqueuePostProc_ACloseTimeoutRunsTheStages$|TestPermanentFaultIn$

# enqueuePostProc reads CloseJobHandles' error: a permanent storage fault
# becomes the run's failure reason through the admission, and anything else,
# a timeout included, is logged and the run goes on.

[the permanent-fault branch never taken]
file internal/app/app.go
--- anchor
	if f := permanentFaultIn(closeErr); f != nil {
--- replace
	if f := permanentFaultIn(closeErr); false && f != nil {
--- end

[the fault is not routed to the admission]
file internal/app/app.go
--- anchor
		app.postProcAdmissions.admit(j, permanentFaultReason(f))
--- replace
		_ = f
--- end

[every close error fails the run, a timeout included]
file internal/app/app.go
--- anchor
	if f := permanentFaultIn(closeErr); f != nil {
--- replace
	if f := permanentFaultIn(closeErr); f != nil || closeErr != nil {
		if f == nil {
			f = storagefault.Classify("close", "", closeErr)
		}
--- end

[the walk stops at the first fault in a joined error]
file internal/app/app.go
--- anchor
			if f := permanentFaultIn(e); f != nil {
				return f
			}
--- replace
			return permanentFaultIn(e)
--- end

[a retryable fault counts as permanent]
file internal/app/app.go
--- anchor
	if f, ok := err.(*storagefault.Fault); ok && f.Permanent { //nolint:errorlint // walks the tree itself, one node at a time
--- replace
	if f, ok := err.(*storagefault.Fault); ok { //nolint:errorlint // walks the tree itself, one node at a time
--- end
