pkg ./internal/assembler/
run ^TestOwnedRanges_(ClaimOverAnOwnedRangePanics|ClaimBetweenAbuttingRangesIsAccepted)$

# claim refuses every intersection, its own owner's included: an owner never
# reaches claim again, so a claim over an owned range is a broken invariant.

[an intersecting entry is tolerated]
file internal/assembler/ranges.go
--- anchor
	if lo < len(o.s) && o.s[lo].r.Off < r.end() {
--- replace
	if false {
--- end

[only an entry starting at or below the claim collides]
file internal/assembler/ranges.go
--- anchor
	if lo < len(o.s) && o.s[lo].r.Off < r.end() {
--- replace
	if lo < len(o.s) && o.s[lo].r.Off <= r.Off {
--- end

[an abutting neighbour collides]
file internal/assembler/ranges.go
--- anchor
	if lo < len(o.s) && o.s[lo].r.Off < r.end() {
--- replace
	if lo < len(o.s) && o.s[lo].r.Off <= r.end() {
--- end
