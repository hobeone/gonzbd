pkg ./internal/job/
run TestRepairStateFrom_EveryBranch

[the two zero-capacity verdicts collapsed]
file internal/job/repair.go
--- anchor
		if hasPar2Files {
			return RepairUnknown
		}
		return RepairNoCapacity
--- replace
		return RepairNoCapacity
--- end

[the damage test no longer comes first]
file internal/job/repair.go
--- anchor
	if contentFailedBytes == 0 {
		return RepairIntact
	}
--- replace
	if contentFailedBytes < 0 {
		return RepairIntact
	}
--- end

[the capacity boundary made exclusive]
file internal/job/repair.go
--- anchor
	if contentFailedBytes > recoveryBytes {
--- replace
	if contentFailedBytes >= recoveryBytes {
--- end
