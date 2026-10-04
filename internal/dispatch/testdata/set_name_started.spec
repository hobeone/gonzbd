pkg ./internal/dispatch/
run TestSetName_RefusesAJobThatHasStarted

[a started job may be renamed]
file internal/dispatch/registry.go
--- anchor
	if e.j.DownloadBegun() {
--- replace
	if false {
--- end

[a retried job (done articles, no stamp) may be renamed]
file internal/job/content.go
--- anchor
	return !p.downloadStarted.IsZero() || p.articlesResolved > p.articlesFailed
--- replace
	return !p.downloadStarted.IsZero()
--- end

[a stamped job may be renamed]
file internal/job/content.go
--- anchor
	return !p.downloadStarted.IsZero() || p.articlesResolved > p.articlesFailed
--- replace
	return p.articlesResolved > p.articlesFailed
--- end

[a job whose articles all failed is refused]
file internal/job/content.go
--- anchor
	return !p.downloadStarted.IsZero() || p.articlesResolved > p.articlesFailed
--- replace
	return !p.downloadStarted.IsZero() || p.articlesResolved > 0
--- end

[a job that merely began an attempt is refused]
file internal/job/content.go
--- anchor
	return !p.downloadStarted.IsZero() || p.articlesResolved > p.articlesFailed
--- replace
	return !p.downloadStarted.IsZero() || p.articlesResolved >= p.articlesFailed
--- end
