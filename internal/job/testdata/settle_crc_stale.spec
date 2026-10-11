pkg ./internal/job/
run TestSettleFileCRC_RefusesAStaleCombine

[a CRC combined from rows that changed between the two sections is installed]
file internal/job/verified.go
--- anchor
	if !sameSlice(j.progress.written[fileIdx], in.rows) {
--- replace
	if false {
--- end

[the rows compared by value, so a drop and identical rewrite passes]
file internal/job/verified.go
--- anchor
	if !sameSlice(j.progress.written[fileIdx], in.rows) {
--- replace
	if !slices.Equal(j.progress.written[fileIdx], in.rows) {
--- end

[sameSlice compares lengths only]
file internal/job/verified.go
--- anchor
	return len(a) == 0 || &a[0] == &b[0]
--- replace
	return true
--- end
