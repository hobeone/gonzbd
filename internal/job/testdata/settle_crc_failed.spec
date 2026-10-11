pkg ./internal/job/
run TestSettleFileCRC_AFailedArticleWithARowIsNoCRC

[the combine outside the lock ignores the failed bits]
file internal/job/verified.go
--- anchor
		failed: j.progress.anyFailed(lo, hi),
--- replace
		failed: false,
--- end

[the settle under the lock ignores the failed bits]
file internal/job/verified.go
--- anchor
	crc, ok := fileCRCFromRows(sortedClone(p.written[fileIdx]), p.anyFailed(lo, hi), lo, hi)
--- replace
	crc, ok := fileCRCFromRows(sortedClone(p.written[fileIdx]), false, lo, hi)
--- end

[the failed scan stops one article short of the range's end]
file internal/job/verified.go
--- anchor
	for i := lo; i < hi; i++ {
		if p.failed.Get(i) {
			return true
--- replace
	for i := lo; i < hi-1; i++ {
		if p.failed.Get(i) {
			return true
--- end
