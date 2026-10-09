pkg ./internal/assembler/
run TestOverlap_PartialRangeOverwritesADurableArticle|TestOwnedRanges_IntersectionIsOwned|TestOwnedRanges_SeededRangeIsOwnedByEveryArrival|TestFileWriter_FaultedWriteOwnsNothing

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
