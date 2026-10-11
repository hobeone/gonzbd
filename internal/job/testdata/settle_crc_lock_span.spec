pkg ./internal/job/
run TestSettleFileCRC_CombinesWithoutTheLock

[contentMu held across the sort and the combine]
file internal/job/verified.go
--- anchor
	// --- No lock held below this line ---
	if j.settleCombineHook != nil {
--- replace
	j.contentMu.Lock()
	defer j.contentMu.Unlock()
	if j.settleCombineHook != nil {
--- end
