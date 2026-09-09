pkg ./internal/dispatch/
run TestRestoreJobMetadata_Coverage

[restore transposes the download stamps it applies to a restored job]
file internal/dispatch/dispatch.go
--- anchor
	j.RestoreProgressState(p.Par2ReleaseReason, started, finished, p.Par2Recovered)
--- replace
	j.RestoreProgressState(p.Par2ReleaseReason, finished, started, p.Par2Recovered)
--- end
