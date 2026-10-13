pkg ./internal/assembler/
run TestOverlap_ContainedOverlapStillCompletesTheFile

[only a shared start offset collides]
file internal/assembler/ranges.go
--- anchor
		if o.s[i].r.intersects(r) {
--- replace
		if o.s[i].r.Off == r.Off {
--- end
