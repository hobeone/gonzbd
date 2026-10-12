pkg ./internal/downloader/
run ^TestClearEmitted_RefusalIsLoggedAtError$

# A gate that drops a request logs a refused ClearArticleEmitted at Error
# rather than discarding it (#286).

[a refused clear is discarded]
file internal/downloader/dispatch.go
--- anchor
	if err := req.job.ClearArticleEmitted(int(req.artIdx)); err != nil {
		d.log.Error("return a gated article to the dispatch pool",
--- replace
	if err := req.job.ClearArticleEmitted(int(req.artIdx)); false && err != nil {
		d.log.Error("return a gated article to the dispatch pool",
--- end
