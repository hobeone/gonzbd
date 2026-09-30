pkg ./internal/job/
run TestMarkArticleFailed_RespectsRepairPolicy$

# MarkArticleFailed's live-download release of a job's deferred recovery
# volumes on a permanent article failure, ignoring Policy.Repair (#653: PP=0
# never enters Repairing, so releasing here only spends bandwidth nothing
# will use). Neutering the guard makes the release happen unconditionally, as
# it did before the fix.

[a permanent article failure always releases, ignoring Policy.Repair]
file internal/job/content.go
--- anchor
		if j.policy.Repair && !j.progress.par2Recovered && j.manifest.RecoveryFiles() > 0 {
--- replace
		if !j.progress.par2Recovered && j.manifest.RecoveryFiles() > 0 {
--- end
