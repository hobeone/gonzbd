pkg ./internal/app/
run TestBuildIngestJob_NZBMetaDefaults$|TestAllMeta$|TestReadNZBMetaPasswords$

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

[allMeta stops matching case-insensitively]
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

[allMeta stops deduplicating multiple values]
file internal/app/ingest.go
--- anchor
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				if !slices.Contains(out, trimmed) {
					out = append(out, trimmed)
				}
			}
--- replace
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				out = append(out, trimmed)
			}
--- end

[readNZBMetaPasswords returns nil even for recorded NZB backup]
file internal/app/app.go
--- anchor
func readNZBMetaPasswords(adminDir, nzbBackup string) []string {
	if nzbBackup == "" {
		return nil
	}
	parsed, err := parseNZBBackup(adminDir, nzbBackup)
--- replace
func readNZBMetaPasswords(adminDir, nzbBackup string) []string {
	if true {
		return nil
	}
	parsed, err := parseNZBBackup(adminDir, nzbBackup)
--- end
