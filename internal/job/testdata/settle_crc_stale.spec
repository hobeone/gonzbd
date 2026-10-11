pkg ./internal/job/
run TestSettleFileCRC_RefusesAStaleCombine

[a CRC combined from rows that changed between the two sections is installed]
file internal/job/verified.go
--- anchor
	if !slices.Equal(j.progress.written[fileIdx], in.rows) {
--- replace
	if false {
--- end
