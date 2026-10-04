pkg ./internal/dispatch/
run TestSetName_RefusesARetriedJobRestoredBeforeHydration

[SetName trusts the restored row without loading progress]
file internal/dispatch/registry.go
--- anchor
	if !ok || j.HasProgress() {
--- replace
	if !ok || j.HasProgress() || true {
--- end

[a restored retried job is not refused once loaded]
file internal/job/content.go
--- anchor
	return !p.downloadStarted.IsZero() || p.articlesResolved > p.articlesFailed
--- replace
	return !p.downloadStarted.IsZero()
--- end
