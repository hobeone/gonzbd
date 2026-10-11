pkg ./internal/app/
run ^(TestStallReason_SaysWhetherAResumeIsNeeded|TestStallReason_FollowsTheLatestFaultsKind)$

[a verification fault carries the plain prefix]
file internal/app/stall.go
--- anchor
	if k == stallVerify {
--- replace
	if false {
--- end

[a retryable fault carries the resume prefix]
file internal/app/stall.go
--- anchor
	if k == stallVerify {
--- replace
	if true {
--- end

[the reason ignores the record's kind]
file internal/app/stall.go
--- anchor
	return r.kind.reasonPrefix() + r.fault
--- replace
	return stallRetryable.reasonPrefix() + r.fault
--- end

[a record keeps its first fault's kind]
file internal/app/stall.go
--- anchor
	rec.kind = kind
--- replace
	if !ok {
		rec.kind = kind
	}
--- end

[a record never leaves stallVerify]
file internal/app/stall.go
--- anchor
	rec.kind = kind
--- replace
	if rec.kind != stallVerify {
		rec.kind = kind
	}
--- end

[a second fault keeps the first fault's text]
file internal/app/stall.go
--- anchor
	rec.fault = fault
--- replace
	if !ok {
		rec.fault = fault
	}
--- end
