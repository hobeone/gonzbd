pkg ./internal/job/
run TestDecodeArticles_RoundTripsMarshalJSON

[a total is persisted beside the articles]
file internal/job/manifest.go
--- anchor
	return json.Marshal(manifestJSON{Articles: articles})
--- replace
	return json.Marshal(struct {
		manifestJSON
		TotalBytes int64 `json:"total_bytes"`
	}{manifestJSON{Articles: articles}, m.ft.totalBytes})
--- end
