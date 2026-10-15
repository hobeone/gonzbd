pkg ./internal/app/
run TestBuildIngestJob_NZBMetaDefaults$

[BuildIngestJob stops falling back to meta category when option is empty]
file internal/app/ingest.go
--- anchor
	category := opts.Category
	if category == "" {
		category = firstMeta(parsed, "category")
	}
--- replace
	category := opts.Category
--- end

[BuildIngestJob stops falling back to meta password when option is empty]
file internal/app/ingest.go
--- anchor
	password := opts.Password
	if password == "" {
		password = firstMeta(parsed, "password")
	}
--- replace
	password := opts.Password
--- end

[firstMeta stops matching case-insensitively]
file internal/app/ingest.go
--- anchor
	for _, k := range keys {
		if strings.EqualFold(k, key) {
			for _, v := range parsed.Meta[k] {
--- replace
	for _, k := range keys {
		if k == key {
			for _, v := range parsed.Meta[k] {
--- end
