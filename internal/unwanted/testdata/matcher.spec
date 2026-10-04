pkg ./internal/unwanted/
run TestUnwanted_|TestNewRules_

[the path component is not stripped]
file internal/unwanted/unwanted.go
--- anchor
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
--- replace
	if i := strings.LastIndexAny(name, `/\`); false && i >= 0 {
--- end

[trailing dots and spaces are not trimmed]
file internal/unwanted/unwanted.go
--- anchor
		return r == '.' || unicode.IsSpace(r) || unicode.IsControl(r)
--- replace
		return false && unicode.IsSpace(r)
--- end

[the comparison is case-sensitive]
file internal/unwanted/unwanted.go
--- anchor
	return strings.ToLower(name[i+1:])
--- replace
	return name[i+1:]
--- end

[a name with no extension is judged like any other]
file internal/unwanted/unwanted.go
--- anchor
	if ext == "" {
		return false
	}
--- replace
	if false {
		return false
	}
--- end

[whitelist mode reads the list as a blacklist]
file internal/unwanted/unwanted.go
--- anchor
	if r.mode == ModeWhitelist {
--- replace
	if false {
--- end

[a pattern error lets the name through]
file internal/unwanted/unwanted.go
--- anchor
	if err != nil {
		// Fail closed in either mode: a check that cannot decide must not
--- replace
	if false && err != nil {
		// Fail closed in either mode: a check that cannot decide must not
--- end

[a pattern error is not reported]
file internal/unwanted/unwanted.go
--- anchor
		if err != nil {
			return false, err
--- replace
		if err != nil {
			return false, nil
--- end

[a malformed configured pattern is accepted]
file internal/unwanted/unwanted.go
--- anchor
		if _, err := path.Match(p, ""); err != nil {
--- replace
		if _, err := path.Match(p, ""); false && err != nil {
--- end

[an entry's leading dot is kept]
file internal/unwanted/unwanted.go
--- anchor
		p := strings.ToLower(strings.TrimLeft(strings.TrimSpace(ext), "."))
--- replace
		p := strings.ToLower(strings.TrimSpace(ext))
--- end

[a trailing format character hides the extension]
file internal/unwanted/unwanted.go
--- anchor
		return r == '.' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
--- replace
		return r == '.' || unicode.IsSpace(r) || unicode.IsControl(r)
--- end

[an entry that can never match is accepted]
file internal/unwanted/unwanted.go
--- anchor
		if strings.ContainsAny(p, ".,") {
--- replace
		if false && strings.ContainsAny(p, ".,") {
--- end

[a comma-joined entry is accepted]
file internal/unwanted/unwanted.go
--- anchor
		if strings.ContainsAny(p, ".,") {
--- replace
		if strings.ContainsAny(p, ".") {
--- end
