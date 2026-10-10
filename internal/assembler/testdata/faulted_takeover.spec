pkg ./internal/assembler/
run TestFaultedIncumbent_TakenOverThenRedeliveryRefused$

[the takeover also resolves the faulted incumbent permanently failed]
file internal/assembler/filewriter.go
--- anchor
		if r.overlaps(off, end) && r.id.sameArticle(id) {
--- replace
		if r.overlaps(off, end) && !r.id.sameArticle(id) {
			w.admitPermanentFailure(r.id.artIdx)
		}
		if r.overlaps(off, end) && r.id.sameArticle(id) {
--- end

[a never-written incumbent settles its range against the rival]
file internal/assembler/filewriter.go
--- anchor
		if r.id.sameArticle(arriving) || !r.written {
--- replace
		if r.id.sameArticle(arriving) {
--- end

[the redelivery is let past the rival's written range]
file internal/assembler/filewriter.go
--- anchor
		if r.id.sameArticle(arriving) || !r.written {
--- replace
		if r.id.sameArticle(arriving) || true {
--- end

[the write fault is not reported to OnArticlesUnwritten]
file internal/assembler/assembler.go
--- anchor
	a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
	a.noteWriteFault(f.info.Path, req, err)
--- replace
	a.noteWriteFault(f.info.Path, req, err)
--- end
