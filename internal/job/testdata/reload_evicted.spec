pkg ./internal/job/
run ^(TestClearEmittedForReload_ReoffersEmittedArticles|TestClearEmittedForReload_UnfailsAnEvictedJob|TestMarkArticleFailedCharge_ResetForgetsTheCharge)$

# #904: a reload un-fails an evicted job's failed articles as it does a
# resident job's, writing the bits alone, and a resident reload re-derives the
# counters whatever it cleared.

[the reload skips an evicted job again]
file internal/job/content.go
--- anchor
	if j.progress == nil {
		return nil, nil
	}
	for fi := range j.progress.files {
--- replace
	if j.manifest == nil || j.progress == nil {
		return nil, nil
	}
	for fi := range j.progress.files {
--- end

[a resident reload recomputes only when it un-failed something]
file internal/job/content.go
--- anchor
	if j.manifest != nil {
		j.progress.recompute(j.manifest)
	}
	return cleared, retained
--- replace
	if j.manifest != nil && (len(cleared) > 0 || len(retained) > 0) {
		j.progress.recompute(j.manifest)
	}
	return cleared, retained
--- end

[a resident reload leaves the counters to the next hydration]
file internal/job/content.go
--- anchor
	if j.manifest != nil {
		j.progress.recompute(j.manifest)
	}
	return cleared, retained
--- replace
	return cleared, retained
--- end

[the reset reads the wrong file's range]
file internal/job/progress.go
--- anchor
func (p *JobProgress) resetForReload(fi int, clearEmitted bool) (cleared, retained []int32) {
	lo, hi := p.fileRange(fi)
--- replace
func (p *JobProgress) resetForReload(fi int, clearEmitted bool) (cleared, retained []int32) {
	lo, hi := p.fileRange(0)
--- end
