pkg ./internal/postproc/
run ^TestDeferredSets_UndescribedEntryIsNotJudged$

# An undescribed par2 entry has no name, so deferredSets leaves it out of the
# Layout B judgement.

[an undescribed entry is judged by its placeholder name]
file internal/postproc/stage_quickcheck.go
--- anchor
		if !fd.Undescribed {
--- replace
		if true {
--- end
