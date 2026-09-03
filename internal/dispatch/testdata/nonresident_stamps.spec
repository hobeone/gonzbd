pkg ./internal/dispatch/
run TestRestoreJobMetadata_Coverage

[restore stops applying download stamps to restored job]
file internal/dispatch/dispatch.go
--- anchor
	_ = j.RestoreDownloadStamps(started, finished)
--- replace
	_, _ = started, finished
--- end
