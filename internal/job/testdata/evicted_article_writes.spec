pkg ./internal/job/
run TestMarkArticleFailed_RecordsAFailureThatArrivesAfterEviction|TestMarkArticleFailed_EvictedRejectsAnOutOfRangeIndex|TestClearArticleEmitted_ReturnsAnEvictedArticleToOutstanding|TestRestoreContent_KeepsWritesMadeWhileEvicted|TestProgressPointerWriters_MatchTheEnumerationStatedInProse

[an evicted job's failure refused, as before the fix]
file internal/job/content.go
--- anchor
	if j.manifest == nil {
		j.progress.setFailedBits(artIdx)
		return nil
	}
--- replace
	if j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
--- end

[an evicted job's failed-bit write neutered]
file internal/job/content.go
--- anchor
		j.progress.setFailedBits(artIdx)
		return nil
--- replace
		return nil
--- end

[the failed bit dropped from the failure's bit transition]
file internal/job/progress.go
--- anchor
	p.done.Set(i)
	p.failed.Set(i)
	p.emitted.Clear(i)
	return true
--- replace
	p.done.Set(i)
	p.emitted.Clear(i)
	return true
--- end

[an evicted job's failure index unbounded]
file internal/job/content.go
--- anchor
	if artIdx < 0 || artIdx >= j.progress.TotalArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.manifest == nil {
		j.progress.setFailedBits(artIdx)
--- replace
	if false {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.manifest == nil {
		j.progress.setFailedBits(artIdx)
--- end

[an evicted job's emitted-clear index unbounded]
file internal/job/content.go
--- anchor
	if artIdx < 0 || artIdx >= j.progress.TotalArticles() {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.manifest == nil {
		j.progress.emitted.Clear(artIdx)
--- replace
	if false {
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.manifest == nil {
		j.progress.emitted.Clear(artIdx)
--- end

[an evicted job's emitted clear refused, as before the fix]
file internal/job/content.go
--- anchor
	if j.manifest == nil {
		j.progress.emitted.Clear(artIdx)
		return nil
	}
--- replace
	if j.manifest == nil {
		return fmt.Errorf("job %s: %w", j.id, ErrNotResident)
	}
--- end

[an evicted job's emitted clear neutered]
file internal/job/content.go
--- anchor
		j.progress.emitted.Clear(artIdx)
		return nil
--- replace
		return nil
--- end

[re-hydration installs a copy of the record, as Hydrate once did]
file internal/job/content.go
--- anchor
	j.progress.recompute(m)
	j.manifest = m
	j.totalBytes = m.TotalBytes()
--- replace
	cp := j.progress.clone()
	cp.recompute(m)
	j.progress = cp
	j.manifest = m
	j.totalBytes = m.TotalBytes()
--- end

[re-hydration replaces the record with a fresh one]
file internal/job/content.go
--- anchor
	j.progress.recompute(m)
	j.manifest = m
	j.totalBytes = m.TotalBytes()
--- replace
	j.progress = newJobProgress(m)
	j.manifest = m
	j.totalBytes = m.TotalBytes()
--- end
