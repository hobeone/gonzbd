pkg ./internal/app/
run TestHandleArticleRejected_RecordsAndPersistsTheFailureOfAnEvictedJob|TestHandleArticlesUnwritten_ClearsTheBitsOfAnEvictedJob|TestArticleHandlers_LogARecordTheJobRefuses|TestAppResidency_RehydrationKeepsAFailureRecordedWhileEvicted

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

[the checkpoint adapter gates failed rows on the lagging counter]
file internal/app/dispatcher_wiring.go
--- anchor
			if p.AnyArticleFailed() {
--- replace
			if p.ArticlesFailed() > 0 {
--- end

[the rejection handler discards the job's error]
file internal/app/durability.go
--- anchor
	if err := j.MarkArticleFailed(int(artIdx)); err != nil {
--- replace
	if err := j.MarkArticleFailed(int(artIdx)); false && err != nil {
--- end

[the roll-back handler discards the job's error]
file internal/app/durability.go
--- anchor
		if err := j.ClearArticleEmitted(int(artIdx)); err != nil {
--- replace
		if err := j.ClearArticleEmitted(int(artIdx)); false && err != nil {
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
