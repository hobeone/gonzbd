pkg ./internal/job/
run TestManifestDoesNotPersistTotalBytes

[the total is persisted again]
file internal/job/manifest.go
--- anchor
	return json.Marshal(manifestJSON{Files: files})
--- replace
	return json.Marshal(struct {
		manifestJSON
		TotalBytes int64 `json:"total_bytes"`
	}{manifestJSON{Files: files}, m.totalBytes})
--- end
