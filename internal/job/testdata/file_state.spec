pkg ./internal/job/
run ^TestJobFileState$

# Job.FileState reads each field of one file's progress, and reports no state
# for a file the job does not have.

[the filename is not read]
file internal/job/verified.go
--- anchor
	return durability.FileState{FileIdx: fi, Complete: f.Complete, Filename: f.Filename, FetchPolicy: uint8(f.Fetch)}, true
--- replace
	return durability.FileState{FileIdx: fi, Complete: f.Complete, FetchPolicy: uint8(f.Fetch)}, true
--- end

[the upper bound is not checked]
file internal/job/verified.go
--- anchor
	if p == nil || fi < 0 || fi >= len(p.files) {
		return durability.FileState{}, false
--- replace
	if p == nil || fi < 0 || fi > len(p.files) {
		return durability.FileState{}, false
--- end
