pkg ./internal/app/
run TestRecorder_PurgeLocked

[the complete override of a kept dirty state dropped]
file internal/app/record.go
--- anchor
		st.Complete = fv.SetComplete
--- replace
		_ = st.Complete
--- end

[an emptied dirty map left in place]
file internal/app/record.go
--- anchor
		st.Complete = fv.SetComplete
		m[fv.FileIdx] = st
	}
	if len(m) == 0 {
--- replace
		st.Complete = fv.SetComplete
		m[fv.FileIdx] = st
	}
	if false {
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
