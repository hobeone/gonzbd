pkg ./internal/app/
run ^TestOverlapRefusal_ChargesOnlyTheUncoveredBytes$

# An overlap refusal reaches the job through handleArticleRejected, which
# charges the uncovered bytes the assembler reported rather than the whole
# article (#432).

[every rejection charges the whole article]
file internal/app/durability.go
--- anchor
	if uncovered == assembler.WholeArticle {
--- replace
	if uncovered == assembler.WholeArticle || true {
--- end
