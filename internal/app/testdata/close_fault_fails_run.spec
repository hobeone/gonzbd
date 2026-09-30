pkg ./internal/app/
run TestEnqueuePostProc_ACloseFaultFailsTheRun$|TestEnqueuePostProc_ACloseFaultIsNotedBehindAnEarlierReason$|TestEnqueuePostProc_ACloseTimeoutRunsTheStages$|TestFaultIn$

# enqueuePostProc reads CloseJobHandles' error: any storage fault, permanent
# or retryable, becomes the run's failure reason through the admission, and
# anything else, a timeout with no fault observed included, is logged and the
# run goes on.

[the fault branch never taken]
file internal/app/app.go
--- anchor
	if f := faultIn(closeErr); f != nil {
--- replace
	if f := faultIn(closeErr); false && f != nil {
--- end

[the fault is not routed to the admission]
file internal/app/app.go
--- anchor
		if app.postProcAdmissions.admit(j, faultReason(f)) == refusedReasonKept {
--- replace
		if false {
--- end

[every close error fails the run, a timeout included]
file internal/app/app.go
--- anchor
	if f := faultIn(closeErr); f != nil {
--- replace
	if f := faultIn(closeErr); f != nil || closeErr != nil {
		if f == nil {
			f = storagefault.Classify("close", "", closeErr)
		}
--- end

[the walk stops at the first fault in a joined error]
file internal/app/app.go
--- anchor
			if f := faultIn(e); f != nil {
				return f
			}
--- replace
			return faultIn(e)
--- end

[a retryable fault is ignored]
file internal/app/app.go
--- anchor
	if f, ok := err.(*storagefault.Fault); ok { //nolint:errorlint // walks the tree itself, one node at a time
--- replace
	if f, ok := err.(*storagefault.Fault); ok && f.Permanent { //nolint:errorlint // walks the tree itself, one node at a time
--- end

[the single-unwrap arm returns nil]
file internal/app/app.go
--- anchor
	case interface{ Unwrap() error }:
		return faultIn(u.Unwrap())
--- replace
	case interface{ Unwrap() error }:
		return nil
--- end
