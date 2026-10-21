pkg ./internal/job/
run TestRecoveryBytesWriters_MatchTheEnumerationStatedInProse

[recoveryBytes writer set mutated]
file internal/job/writer_enumeration_test.go
--- anchor
	want := []string{"AttachFiles", "SetRecoveryBytes"}
--- replace
	want := []string{"AttachFiles"}
--- end
