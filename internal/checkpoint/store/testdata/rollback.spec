pkg ./internal/checkpoint/store/
run TestSaveBatch_RollsBackTheWholeBatch

# Scoped to the one test on purpose. In savebatch.spec this same mutation is
# KILLED by TestSaveBatch_ReportsAMissingRow, which runs first and fails on the
# swallowed error — so that run says nothing about whether the ROLLBACK is
# pinned. Running the atomicity test alone is what makes the verdict about
# atomicity.
[a per-job failure no longer aborts the batch]
file internal/checkpoint/store/store.go
--- anchor
		if err := saveOne(ctx, stmt, cp); err != nil {
--- replace
		if err := saveOne(ctx, stmt, cp); false {
--- end
