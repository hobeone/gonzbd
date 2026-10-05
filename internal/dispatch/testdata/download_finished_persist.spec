pkg ./internal/dispatch/
run TestPersist_RecordsTheDownloadFinishOnlyOutsideFetching

[a row at Fetching records the finish]
file internal/dispatch/tick.go
--- anchor
!df.IsZero() && s.State.State != job.Fetching {
--- replace
!df.IsZero() {
--- end

[a row outside Fetching never records the finish]
file internal/dispatch/tick.go
--- anchor
!df.IsZero() && s.State.State != job.Fetching {
--- replace
!df.IsZero() && s.State.State == job.Fetching {
--- end
