pkg ./internal/assembler/
run ^(TestOwnedRanges_Covered|TestOverlap_RefusalReportsTheUncoveredBytes)$

# An overlap refusal reports the bytes of the arriving article that no other
# article's range covers, so the caller charges par2 for those alone (#432).

[the refusal reports the article's whole length]
file internal/assembler/assembler.go
--- anchor
		uncovered := rng.Len - f.w.owned.covered(rng)
--- replace
		uncovered := rng.Len
--- end

[the rejection route drops the reported count]
file internal/assembler/assembler.go
--- anchor
			a.opts.OnArticleRejected(req.JobID, req.FileIdx, req.ArtIdx, o.reason, o.uncovered)
--- replace
			a.opts.OnArticleRejected(req.JobID, req.FileIdx, req.ArtIdx, o.reason, WholeArticle)
--- end

[an out-of-range offset reports nothing uncovered]
file internal/assembler/assembler.go
--- anchor
		return outcome{disposition: dispositionRejected, reason: reason, uncovered: WholeArticle}
--- replace
		return outcome{disposition: dispositionRejected, reason: reason, uncovered: 0}
--- end

[an owner's range is not clipped to the arriving range]
file internal/assembler/ranges.go
--- anchor
		if lo, hi := max(o.s[i].r.Off, r.Off), min(o.s[i].r.end(), r.end()); hi > lo {
--- replace
		if lo, hi := o.s[i].r.Off, o.s[i].r.end(); hi > lo {
--- end
