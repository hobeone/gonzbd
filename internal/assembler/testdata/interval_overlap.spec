pkg ./internal/assembler/
run Test(Overlap_.*|AcceptedRange_Overlaps|FileWriter_RecordAccepted_MaintainsSortedDisjointIntervals|FileWriter_OffsetSettledBy|FileWriter_RolledBackOwnerKeepsItsOffsetUntilReplaced|Collision_.*)$

[overlaps treats zero-length point at different start offset as overlapping covering range]
file internal/assembler/filewriter.go
--- anchor
	if end == off || r.end == r.off {
		return false
	}
--- replace
	if false {
		return false
	}
--- end

[overlaps treats off < r.off as never overlapping]
file internal/assembler/filewriter.go
--- anchor
	if off < r.off {
		return r.off < end
	}
--- replace
	if off < r.off {
		return false
	}
--- end

[overlaps shifts r.off < end boundary to r.off <= end so abutting ranges collide]
file internal/assembler/filewriter.go
--- anchor
	if off < r.off {
		return r.off < end
	}
--- replace
	if off < r.off {
		return r.off <= end
	}
--- end

[overlaps treats off > r.off as never overlapping]
file internal/assembler/filewriter.go
--- anchor
	return off < r.end
--- replace
	return false
--- end

[overlaps shifts off < r.end boundary to off <= r.end so abutting ranges collide]
file internal/assembler/filewriter.go
--- anchor
	return off < r.end
--- replace
	return off <= r.end
--- end

[offsetSettledBy settles against a never-written incumbent]
file internal/assembler/filewriter.go
--- anchor
		if r.id.sameArticle(arriving) || !r.written {
--- replace
		if r.id.sameArticle(arriving) {
--- end

[offsetSettledBy lets an arrival past a written incumbent]
file internal/assembler/filewriter.go
--- anchor
		if r.id.sameArticle(arriving) || !r.written {
--- replace
		if r.id.sameArticle(arriving) || true {
--- end

[recordAccepted merges a different article's unwritten range instead of dropping it]
file internal/assembler/filewriter.go
--- anchor
		if r.overlaps(off, end) && r.id.sameArticle(id) {
--- replace
		if r.overlaps(off, end) {
--- end

[firstCandidateIdx omits predecessor idx-1 check]
file internal/assembler/filewriter.go
--- anchor
	if idx > 0 {
		return idx - 1
	}
--- replace
	if false {
		return idx - 1
	}
--- end

[noteWritten omits latching written on accepted interval]
file internal/assembler/filewriter.go
--- anchor
		if w.accepted[i].overlaps(off, end) && w.accepted[i].id.sameArticle(id) {
			w.accepted[i].written = true
			break
		}
--- replace
		if w.accepted[i].overlaps(off, end) && w.accepted[i].id.sameArticle(id) {
			break
		}
--- end

[offsetSettledBy ignores decoded length and probes zero-length point]
file internal/assembler/assembler.go
--- anchor
	if _, settled := f.w.offsetSettledBy(req.Offset, int64(len(req.Data)), id); settled {
--- replace
	if _, settled := f.w.offsetSettledBy(req.Offset, 0, id); settled {
--- end

[offsetSettledBy neutered in acceptArticle]
file internal/assembler/assembler.go
--- anchor
	if _, settled := f.w.offsetSettledBy(req.Offset, int64(len(req.Data)), id); settled {
--- replace
	if _, settled := f.w.offsetSettledBy(req.Offset, int64(len(req.Data)), id); settled && false {
--- end

[recordAccepted fails to subsume interior zero-length point into covering interval]
file internal/assembler/filewriter.go
--- anchor
	for last < len(w.accepted) && (w.accepted[last].overlaps(off, end) || w.accepted[last].off < end) {
--- replace
	for last < len(w.accepted) && w.accepted[last].overlaps(off, end) {
--- end

[recordAccepted drops written latch on same-article re-accept]
file internal/assembler/filewriter.go
--- anchor
			written = written || r.written
--- replace
			written = false
--- end

[recordAccepted neutered in Accept]
file internal/assembler/filewriter.go
--- anchor
	w.recordAccepted(id, off, off+int64(len(data)))
--- replace
	_ = id
--- end
