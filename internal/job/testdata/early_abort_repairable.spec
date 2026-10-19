pkg ./internal/job/
run ^(TestCheckEarlyAbort_SparesAJobWhoseDamageFitsRecovery|TestCheckEarlyAbort_SparingLeavesTheLatchUnset)$

[the rate rule ignores the repair state]
file internal/job/content.go
--- anchor
	if st := j.repairStateLocked(); st == RepairPossible || st == RepairIntact {
--- replace
	if false {
--- end

[a job with intact content is no longer spared]
file internal/job/content.go
--- anchor
	if st := j.repairStateLocked(); st == RepairPossible || st == RepairIntact {
--- replace
	if st := j.repairStateLocked(); st == RepairPossible {
--- end

[a job whose damage fits recovery is no longer spared]
file internal/job/content.go
--- anchor
	if st := j.repairStateLocked(); st == RepairPossible || st == RepairIntact {
--- replace
	if st := j.repairStateLocked(); st == RepairIntact {
--- end

[a spared call latches the rule]
file internal/job/content.go
--- anchor
	if st := j.repairStateLocked(); st == RepairPossible || st == RepairIntact {
		return false
--- replace
	if st := j.repairStateLocked(); st == RepairPossible || st == RepairIntact {
		j.progress.earlyAborted = true
		return false
--- end
