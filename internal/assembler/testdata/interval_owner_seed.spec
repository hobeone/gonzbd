pkg ./internal/assembler/
run TestOwnedRanges_SeedMergesIntersectingRanges|TestOwnedRanges_SeedOnNonEmptySetChangesNothing

[seed claims each range separately]
file internal/assembler/ranges.go
--- anchor
	for _, r := range sorted {
		if n := len(o.s); n > 0 && r.Off <= o.s[n-1].r.end() {
--- replace
	for _, r := range sorted {
		o.claim(r, seededOwner)
		continue
		if n := len(o.s); n > 0 && r.Off <= o.s[n-1].r.end() {
--- end

[seed accepts a non-empty set]
file internal/assembler/ranges.go
--- anchor
	if len(o.s) != 0 {
		return errSeedNotEmpty
--- replace
	if false {
		return errSeedNotEmpty
--- end
