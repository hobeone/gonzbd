pkg ./internal/par2/
run ^(TestIdentification_SliceSize|TestAssessment_DeliveredIndices)$

[slice size ignores a second SetID]
file internal/par2/identify.go
--- anchor
		if s.id != first.id || s.sliceSize != first.sliceSize {
--- replace
		if s.sliceSize != first.sliceSize {
--- end

[slice size ignores a differing size]
file internal/par2/identify.go
--- anchor
		if s.id != first.id || s.sliceSize != first.sliceSize {
--- replace
		if s.id != first.id {
--- end

[slice size accepts zero]
file internal/par2/identify.go
--- anchor
	if first.sliceSize == 0 {
--- replace
	if false {
--- end

[undelivered file reports position 0]
file internal/par2/assess.go
--- anchor
			out[i] = -1
--- replace
			out[i] = 0
--- end

[join reports the identification position]
file internal/par2/assess.go
--- anchor
		out[i] = d.pos
--- replace
		out[i] = i + d.pos*0
--- end

[index records the wrong position]
file internal/par2/assess.go
--- anchor
		d := &delivered{crc: af.CRC32, pos: i}
--- replace
		d := &delivered{crc: af.CRC32, pos: i * 0}
--- end
