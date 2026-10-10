pkg ./internal/assembler/
run Test(Overlap_.*|OwnedRanges_ArrivalVerdicts|OwnedRanges_IntersectionIsOwned|FileWriter_FaultedWriteOwnsNothing|Collision_.*)$

[intersects treats a zero-length range as occupying bytes]
file internal/assembler/ranges.go
--- anchor
	return r.Len > 0 && s.Len > 0 && r.Off < s.end() && s.Off < r.end()
--- replace
	return r.Off < s.end() && s.Off < r.end()
--- end

[ownerOf starts its scan at the first range starting at or after the probe]
file internal/assembler/ranges.go
--- anchor
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.end() > r.Off })
	for ; i < len(o.s) && o.s[i].r.Off < r.end(); i++ {
--- replace
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.Off >= r.Off })
	for ; i < len(o.s) && o.s[i].r.Off < r.end(); i++ {
--- end

[ownerOf stops before a range starting inside the probe]
file internal/assembler/ranges.go
--- anchor
	for ; i < len(o.s) && o.s[i].r.Off < r.end(); i++ {
--- replace
	for ; i < len(o.s) && o.s[i].r.Off <= r.Off; i++ {
--- end

[claim records an empty range]
file internal/assembler/ranges.go
--- anchor
	if r.Len <= 0 {
		return
	}
	lo := sort.Search(
--- replace
	if false {
		return
	}
	lo := sort.Search(
--- end

[writeOne never claims the range it wrote]
file internal/assembler/filewriter.go
--- anchor
	w.owned.claim(Range{Off: off, Len: int64(len(data))}, id)
	w.noteWritten(id)
--- replace
	w.noteWritten(id)
--- end

[acceptArticle probes a zero-length range]
file internal/assembler/assembler.go
--- anchor
	if _, settled := f.w.owned.ownerOf(Range{Off: req.Offset, Len: int64(len(req.Data))}, id); settled {
--- replace
	if _, settled := f.w.owned.ownerOf(Range{Off: req.Offset, Len: 0}, id); settled {
--- end

[ownerOf neutered in acceptArticle]
file internal/assembler/assembler.go
--- anchor
	if _, settled := f.w.owned.ownerOf(Range{Off: req.Offset, Len: int64(len(req.Data))}, id); settled {
--- replace
	if _, settled := f.w.owned.ownerOf(Range{Off: req.Offset, Len: int64(len(req.Data))}, id); settled && false {
--- end
