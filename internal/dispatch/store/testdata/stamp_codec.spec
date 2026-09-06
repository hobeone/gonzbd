pkg ./internal/dispatch/store/
run TestStore_AllHeaderFieldsAndTimestampsSurviveRoundTrip

[Save stops persisting download_started]
file internal/dispatch/store/store.go
--- anchor
		p.Header.Added, p.DownloadStarted, p.DownloadFinished, p.Par2ReleaseReason,
--- replace
		p.Header.Added, int64(0), p.DownloadFinished, p.Par2ReleaseReason,
--- end

[Save stops persisting download_finished]
file internal/dispatch/store/store.go
--- anchor
		p.Header.Added, p.DownloadStarted, p.DownloadFinished, p.Par2ReleaseReason,
--- replace
		p.Header.Added, p.DownloadStarted, int64(0), p.Par2ReleaseReason,
--- end
