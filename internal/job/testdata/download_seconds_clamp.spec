pkg ./internal/job/
run TestDownloadSeconds

[the one-second floor removed from DownloadSeconds]
file internal/job/progress.go
--- anchor
	return max(int64(p.downloadFinished.Sub(p.downloadStarted).Seconds()), 1)
--- replace
	return max(int64(p.downloadFinished.Sub(p.downloadStarted).Seconds()), -1<<62)
--- end
