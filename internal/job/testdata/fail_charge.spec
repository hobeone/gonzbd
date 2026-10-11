pkg ./internal/job/
run ^(TestMarkArticleFailedUncovered_|TestClone_DoesNotShareTheChargeMap$)

# A failed article whose range other articles partly or wholly wrote is charged
# only its uncovered bytes (#432). chargeOf is the one computation of that
# charge; each mutation below breaks one reader or writer of it.

[the recorded charge is ignored]
file internal/job/progress.go
--- anchor
	if c, ok := p.failCharge[i]; ok {
--- replace
	if c, ok := p.failCharge[i]; false && ok {
--- end

[the charge is not capped at the article's size]
file internal/job/progress.go
--- anchor
		return min(c, full)
--- replace
		return c
--- end

[the whole-article sentinel is recorded as a charge]
file internal/job/progress.go
--- anchor
	if uncovered == wholeArticle {
--- replace
	if false {
--- end

[the live failure records no charge]
file internal/job/progress.go
--- anchor
	p.limitCharge(i, uncovered)
--- replace
	_ = uncovered
--- end

[the live failure counts no covered bytes as downloaded]
file internal/job/progress.go
--- anchor
	p.files[fi].BytesDownloaded += int64(m.ArticleBytes(i)) - charge
--- replace
	p.files[fi].BytesDownloaded += 0
--- end

[recompute charges the whole article]
file internal/job/progress.go
--- anchor
					charge = p.chargeOf(m, i)
--- replace
					charge = int64(m.ArticleBytes(i))
--- end

[recompute counts no covered bytes as downloaded]
file internal/job/progress.go
--- anchor
				downloaded += int64(m.ArticleBytes(i)) - charge
--- replace
				if !p.failed.Get(i) {
					downloaded += int64(m.ArticleBytes(i))
				}
--- end

[a reload unwinds the whole article]
file internal/job/progress.go
--- anchor
		p.failedBytes -= charge
--- replace
		p.failedBytes -= int64(m.ArticleBytes(i))
--- end

[a reload leaves the covered bytes downloaded]
file internal/job/progress.go
--- anchor
		p.files[fi].BytesDownloaded -= int64(m.ArticleBytes(i)) - charge
--- replace
		p.files[fi].BytesDownloaded -= 0
--- end

[clearing the failed bit keeps the charge]
file internal/job/progress.go
--- anchor
		delete(p.failCharge, i)
--- replace
		_ = i
--- end

[a clone shares the charge map]
file internal/job/progress.go
--- anchor
	cp.failCharge = maps.Clone(p.failCharge)
--- replace
	cp.failCharge = p.failCharge
--- end

[an evicted failure records no charge]
file internal/job/content.go
--- anchor
			j.progress.limitCharge(artIdx, uncovered)
--- replace
			_ = uncovered
--- end

[a failure charging nothing releases recovery volumes]
file internal/job/content.go
--- anchor
	if charge, ok := j.progress.markFailedUncovered(j.manifest, artIdx, uncovered); ok && charge > 0 {
--- replace
	if charge, ok := j.progress.markFailedUncovered(j.manifest, artIdx, uncovered); ok || charge > 0 {
--- end

[a negative count is accepted]
file internal/job/content.go
--- anchor
	if uncovered < 0 {
--- replace
	if false {
--- end
