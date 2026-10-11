pkg ./internal/assembler/
run TestOwnedRanges_ClaimKeepsOffsetOrder$

# claim inserts each range at its offset position, so ownerOf's binary search,
# which assumes the slice is sorted, finds every entry's owner.

[claim appends instead of inserting in offset order]
file internal/assembler/ranges.go
--- anchor
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.Off >= r.Off })
--- replace
	i := len(o.s)
--- end

[claim prepends instead of inserting in offset order]
file internal/assembler/ranges.go
--- anchor
	i := sort.Search(len(o.s), func(i int) bool { return o.s[i].r.Off >= r.Off })
--- replace
	i := 0
--- end
