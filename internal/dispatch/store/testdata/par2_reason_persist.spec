pkg ./internal/dispatch/store/
run TestStore_AllHeaderFieldsAndTimestampsSurviveRoundTrip

[Save stops persisting par2_release_reason]
file internal/dispatch/store/store.go
--- anchor
		p.Header.Added, p.DownloadStarted, p.DownloadFinished, p.Par2ReleaseReason,
--- replace
		p.Header.Added, p.DownloadStarted, p.DownloadFinished, "",
--- end
