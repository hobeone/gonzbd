pkg ./internal/heldbytes/
run TestBudget_AccountingReturnsToZero|TestBudget_BlockedAcquirerUnblocksOnRelease|TestBudget_BlockedAcquirerUnblocksOnContextCancel|TestBudget_OversizedAcquireProceedsWhenNothingIsHeld|TestHold_ReleaseIsIdempotent|TestNew_RefusesANonPositiveLimit

[an oversized acquire is not clamped to the whole budget, so it waits forever]
file internal/heldbytes/heldbytes.go
--- anchor
	n = min(max(n, 0), b.limit)
--- replace
	n = max(n, 0)
--- end

[a second release of one hold gives its bytes back again]
file internal/heldbytes/heldbytes.go
--- anchor
	if h == nil || !h.released.CompareAndSwap(false, true) {
--- replace
	if h == nil {
--- end
