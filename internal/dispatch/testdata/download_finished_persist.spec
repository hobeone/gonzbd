pkg ./internal/dispatch/
run TestPersist_RecordsTheDownloadFinishOnlyOutsideFetching

[a row at Fetching records the finish]
file internal/dispatch/tick.go
--- anchor
!df.IsZero() && (s.State.State != job.Fetching || s.State.Next != job.StateUnset) {
--- replace
!df.IsZero() {
--- end

[a row outside Fetching never records the finish]
file internal/dispatch/tick.go
--- anchor
!df.IsZero() && (s.State.State != job.Fetching || s.State.Next != job.StateUnset) {
--- replace
!df.IsZero() && s.State.State == job.Fetching {
--- end

[a row at Fetching withholds the finish even with the report recorded]
file internal/dispatch/tick.go
--- anchor
!df.IsZero() && (s.State.State != job.Fetching || s.State.Next != job.StateUnset) {
--- replace
!df.IsZero() && s.State.State != job.Fetching {
--- end
