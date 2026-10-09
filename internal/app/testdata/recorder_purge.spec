pkg ./internal/app/
run TestRecorder_PurgeLocked

[the complete-verdict dirty purge dropped]
file internal/app/record.go
--- anchor
	if fv.DeleteAll || fv.SetComplete || fv.ClearComplete {
--- replace
	if fv.DeleteAll {
--- end

[the named-art row filter widened to the whole file]
file internal/app/record.go
--- anchor
		if row.FileIdx == fv.FileIdx && (fv.DeleteAll || drop[row.ArtIdx]) {
--- replace
		if row.FileIdx == fv.FileIdx {
--- end

[the file filter dropped]
file internal/app/record.go
--- anchor
		if row.FileIdx == fv.FileIdx && (fv.DeleteAll || drop[row.ArtIdx]) {
--- replace
		if fv.DeleteAll || drop[row.ArtIdx] {
--- end
