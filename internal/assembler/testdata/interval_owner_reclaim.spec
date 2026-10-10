pkg ./internal/assembler/
run TestOwnedRanges_ReclaimBySameArticleLeavesOneEntry|TestOwnedRanges_ClaimOverAnotherArticlePanics

[same-article entries are appended, not replaced]
file internal/assembler/ranges.go
--- anchor
	o.s = slices.Delete(o.s, lo, hi)
--- replace
	o.s = slices.Delete(o.s, lo, lo)
--- end

[a different article's intersecting entry is tolerated]
file internal/assembler/ranges.go
--- anchor
		if !o.s[hi].id.sameArticle(id) {
--- replace
		if false {
--- end
