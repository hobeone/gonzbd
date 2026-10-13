pkg ./internal/assembler/
run TestOwnedRanges_ClaimKeepsOffsetOrder$

# claim inserts each range at its offset position, so ownerOf's binary search,
# which assumes the slice is sorted, finds every entry's owner.

[claim appends instead of inserting in offset order]
file internal/assembler/ranges.go
--- anchor
	o.s = slices.Insert(o.s, lo, ownedRange{r: r, id: id})
--- replace
	o.s = slices.Insert(o.s, len(o.s), ownedRange{r: r, id: id})
--- end

[claim prepends instead of inserting in offset order]
file internal/assembler/ranges.go
--- anchor
	o.s = slices.Insert(o.s, lo, ownedRange{r: r, id: id})
--- replace
	o.s = slices.Insert(o.s, 0, ownedRange{r: r, id: id})
--- end
