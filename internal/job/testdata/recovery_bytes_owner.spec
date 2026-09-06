pkg ./internal/job/
run TestRecoveryBytesWriters_MatchTheEnumerationStatedInProse

[recoveryBytes writer set mutated]
file internal/job/writer_enumeration_test.go
--- anchor
	want := []string{"AttachContent", "RestoreContent", "SetRecoveryBytes"}
--- replace
	want := []string{"AttachContent", "RestoreContent"}
--- end
