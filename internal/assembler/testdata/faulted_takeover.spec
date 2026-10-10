pkg ./internal/assembler/
run TestFaultedIncumbent_TakenOverThenRedeliveryRefused$

[a faulted incumbent claims its range before the write returns]
file internal/assembler/filewriter.go
--- anchor
	_, err := w.writeAt(data, off)
--- replace
	w.owned.claim(Range{Off: off, Len: int64(len(data))}, id)
	_, err := w.writeAt(data, off)
--- end

[the redelivery is let past the rival's written range]
file internal/assembler/ranges.go
--- anchor
		if o.s[i].r.intersects(r) && !o.s[i].id.sameArticle(arriving) {
--- replace
		if false {
--- end

[the write fault is not reported to OnArticlesUnwritten]
file internal/assembler/assembler.go
--- anchor
	a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
	a.noteWriteFault(f.info.Path, req, err)
--- replace
	a.noteWriteFault(f.info.Path, req, err)
--- end
