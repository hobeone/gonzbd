pkg ./internal/job/
run TestAttachFiles_SetsRecoveryBytesFromTheTable

[AttachFiles does not derive recoveryBytes from the table]
file internal/job/content.go
--- anchor
	j.recoveryBytes = ft.RecoveryBytes()
--- replace
	j.recoveryBytes = 0
--- end
