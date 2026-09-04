pkg ./internal/dispatch/
run TestRestoreJobMetadata_ZeroAddedDoesNotResetToEpoch

[restore resets Added to epoch unconditionally]
file internal/dispatch/dispatch.go
--- anchor
	if p.Header.Added > 0 {
		j.SetAdded(time.Unix(p.Header.Added, 0).UTC())
	}
--- replace
	j.SetAdded(time.Unix(p.Header.Added, 0).UTC())
--- end
