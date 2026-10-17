pkg ./internal/job/
run ^(TestMarkArticleFailedCharge_|TestCheckEarlyAbort_IgnoresARefusalThatChargesNothing$|TestCheckEarlyAbort_AZeroChargeRefusalStaysResolved$|TestClone_DoesNotShareTheChargeMap$)

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
	if uncovered == WholeArticle {
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
			j.progress.limitCharge(artIdx, charge)
--- replace
			_ = charge
--- end

[a live charge is recorded before the failed bit is known to be new]
file internal/job/progress.go
--- anchor
	if !p.setFailedBits(i) {
		return false
	}
	p.limitCharge(i, uncovered)
--- replace
	p.limitCharge(i, uncovered)
	if !p.setFailedBits(i) {
		return false
	}
--- end

[an evicted charge is recorded on an article already failed]
file internal/job/content.go
--- anchor
		if j.progress.setFailedBits(artIdx) {
			j.progress.limitCharge(artIdx, charge)
		}
--- replace
		j.progress.setFailedBits(artIdx)
		j.progress.limitCharge(artIdx, charge)
--- end

[a negative charge is accepted]
file internal/job/content.go
--- anchor
	if charge < 0 && charge != WholeArticle {
--- replace
	if false {
--- end

[the whole-article charge is refused as negative]
file internal/job/content.go
--- anchor
	if charge < 0 && charge != WholeArticle {
--- replace
	if charge < 0 {
--- end

# A failure charged nothing is resolved but not failed for the early-abort
# rate (#844). Both writers of articlesFailed apply the rule.

[the early-abort rate counts a zero charge as failed, live]
file internal/job/progress.go
--- anchor
	if charge > 0 {
		p.articlesFailed++
	}
--- replace
	if charge >= 0 {
		p.articlesFailed++
	}
--- end

[the early-abort rate counts a zero charge as failed, recomputed]
file internal/job/progress.go
--- anchor
					if charge > 0 {
						failed++
					}
--- replace
					if charge >= 0 {
						failed++
					}
--- end

# The same refusal stays resolved: it is in the rate's denominator.

[the early-abort rate leaves a zero charge unresolved, live]
file internal/job/progress.go
--- anchor
	p.articlesResolved++
	if charge > 0 {
		p.articlesFailed++
	}
--- replace
	if charge > 0 {
		p.articlesResolved++
		p.articlesFailed++
	}
--- end

[the early-abort rate leaves a zero charge unresolved, recomputed]
file internal/job/progress.go
--- anchor
			if p.done.Get(i) {
				resolved++
--- replace
			if p.done.Get(i) {
				if !p.failed.Get(i) || p.chargeOf(m, i) > 0 {
					resolved++
				}
--- end
