pkg ./internal/assembler/
run TestOverlap_PartialRangeOverlapIsRefused|TestOwnedRanges_IntersectionIsOwned|TestOwnedRanges_SeededRangeIsOwnedByEveryArrival|TestFileWriter_FaultedWriteOwnsNothing|TestOverlap_ArrivalStartingBeforeTheIncumbentIsRefused|TestOverlap_ZeroLengthArrivalInsideOwnedRangeIsAccepted

[intersection never reported]
file internal/assembler/ranges.go
--- anchor
		if o.s[i].r.intersects(r) && !o.s[i].id.sameArticle(arriving) {
--- replace
		if false {
--- end

[seed claims as the zero article]
file internal/assembler/ranges.go
--- anchor
		o.s = append(o.s, ownedRange{r: r, id: seededOwner})
--- replace
		o.s = append(o.s, ownedRange{r: r, id: articleID{}})
--- end

[range claimed before the write returned]
file internal/assembler/filewriter.go
--- anchor
	_, err := w.writeAt(data, off)
--- replace
	w.owned.claim(Range{Off: off, Len: int64(len(data))}, id)
	_, err := w.writeAt(data, off)
--- end

[acceptArticle probes only the arrival's first byte]
file internal/assembler/assembler.go
--- anchor
	if _, settled := f.w.owned.ownerOf(Range{Off: req.Offset, Len: int64(len(req.Data))}, id); settled {
--- replace
	if _, settled := f.w.owned.ownerOf(Range{Off: req.Offset, Len: min(int64(len(req.Data)), 1)}, id); settled {
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
