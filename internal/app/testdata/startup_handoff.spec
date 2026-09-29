pkg ./internal/app/
run TestStart_HandsOffOnlyCompleteJobsWithNoVerdict

# Which restored jobs Start hands to post-processing itself. Each clause of
# startupHandOffs' test is neutered on its own, then the loop's use of it.
#
# Where startupHandOffs runs (before the first tick) has no mutation here. A
# placement after the ticker starts differs only for a job a download completes
# between the tick and the choice, and no seam in this package can drive a
# download to completion there.

[a job whose download-complete report is recorded is handed off too]
file internal/app/startup_reconcile.go
--- anchor
		if v.Next != job.StateUnset || (v.State != job.StateUnset && v.State != job.Fetching) {
--- replace
		if v.State != job.StateUnset && v.State != job.Fetching {
--- end

[a job at Fetching with no verdict is not handed off]
file internal/app/startup_reconcile.go
--- anchor
		if v.Next != job.StateUnset || (v.State != job.StateUnset && v.State != job.Fetching) {
--- replace
		if v.Next != job.StateUnset || v.State != job.StateUnset {
--- end

[a never-run job is not handed off]
file internal/app/startup_reconcile.go
--- anchor
		if v.Next != job.StateUnset || (v.State != job.StateUnset && v.State != job.Fetching) {
--- replace
		if v.Next != job.StateUnset || v.State != job.Fetching {
--- end

[an incomplete job is handed off]
file internal/app/startup_reconcile.go
--- anchor
		if j, ok := app.dispatcher.Job(row.ID); ok && j.IsComplete() {
--- replace
		if _, ok := app.dispatcher.Job(row.ID); ok {
--- end

[Start ignores the choice and hands off nothing]
file internal/app/app.go
--- anchor
			if !handOffs[row.ID] {
				continue
			}
--- replace
			if !handOffs[row.ID] || true {
				continue
			}
--- end
