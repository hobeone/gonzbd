pkg ./internal/app/
run TestHandleArticleRejected_RecordsTheFailureOfAnEvictedJob|TestHandleArticlesUnwritten_ClearsTheBitsOfAnEvictedJob|TestArticleHandlers_LogARecordTheJobRefuses|TestAppResidency_RehydrationKeepsAFailureRecordedWhileEvicted

[an evicted job's failure refused, as before the fix]
file internal/job/content.go
--- anchor
	if j.manifest == nil {
		if j.progress.setFailedBits(artIdx) {
			j.progress.limitCharge(artIdx, charge)
		}
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

[the rejection handler discards the job's error]
file internal/app/durability.go
--- anchor
	if err := j.MarkArticleFailed(int(artIdx), charge); err != nil {
		app.log.Warn("record rejected article as permanently failed",
--- replace
	if err := j.MarkArticleFailed(int(artIdx), charge); false && err != nil {
		app.log.Warn("record rejected article as permanently failed",
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
	out := make([]FileInstall, len(vs))
--- replace
	j.progress = newJobProgress(m.Files(), nil)
	j.manifest = m
	out := make([]FileInstall, len(vs))
--- end
