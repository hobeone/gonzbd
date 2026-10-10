pkg ./internal/assembler/
run TestOverlap_PartialRangeOverwritesADurableArticle

[only a shared start offset collides]
file internal/assembler/ranges.go
--- anchor
		if o.s[i].r.intersects(r) && !o.s[i].id.sameArticle(arriving) {
--- replace
		if o.s[i].r.Off == r.Off && !o.s[i].id.sameArticle(arriving) {
--- end
