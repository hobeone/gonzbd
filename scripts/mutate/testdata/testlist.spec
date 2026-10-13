# Red check for the static test listing (scripts/mutate/testlist.go): each
# mutation breaks one rule `go test -list` applies, and the differential tests
# against `go test -list` must die.
#
#     go run ./scripts/mutate scripts/mutate/testdata/testlist.spec
pkg ./scripts/mutate/
run TestListTests|TestStaticDir|TestListingEnv
timeout 5m

[a lower-case letter after Test still names a test]
file scripts/mutate/testlist.go
--- anchor
	return !unicode.IsLower(r)
--- replace
	return !unicode.IsLower(r) || r != 0
--- end

[TestMain taking *testing.M is read as a test]
file scripts/mutate/testlist.go
--- anchor
		if name == "TestMain" && !takesPointerTo(fn, "T") {
--- replace
		if false && name == "TestMain" && !takesPointerTo(fn, "T") {
--- end

[a method named like a test is listed]
file scripts/mutate/testlist.go
--- anchor
		if !ok || fn.Recv != nil {
--- replace
		if !ok {
--- end

[the external test package is not read]
file scripts/mutate/testlist.go
--- anchor
	for _, f := range slices.Concat(p.TestGoFiles, p.XTestGoFiles) {
--- replace
	for _, f := range slices.Concat(p.TestGoFiles) {
--- end

[the spec's tags are ignored]
file scripts/mutate/testlist.go
--- anchor
	ctx.BuildTags = strings.FieldsFunc(tags, func(r rune) bool { return r == ',' || r == ' ' })
--- replace
	ctx.BuildTags = strings.FieldsFunc(tags[:0], func(r rune) bool { return r == ',' || r == ' ' })
--- end

[a comma-separated tag list is read as one tag]
file scripts/mutate/testlist.go
--- anchor
	ctx.BuildTags = strings.FieldsFunc(tags, func(r rune) bool { return r == ',' || r == ' ' })
--- replace
	ctx.BuildTags = strings.FieldsFunc(tags, func(r rune) bool { return r == ' ' })
--- end

[the go command's GOOS is ignored]
file scripts/mutate/testlist.go
--- anchor
	ctx.GOOS, ctx.GOARCH = e.GOOS, e.GOARCH
--- replace
	ctx.GOOS, ctx.GOARCH = build.Default.GOOS, e.GOARCH
--- end

[the go command's CGO_ENABLED is ignored]
file scripts/mutate/testlist.go
--- anchor
	ctx.CgoEnabled = e.CGO_ENABLED == "1"
--- replace
	ctx.CgoEnabled = e.CGO_ENABLED != "x"
--- end

[a ... pattern is read as a directory]
file scripts/mutate/testlist.go
--- anchor
	if strings.Contains(pkg, "...") {
--- replace
	if false && strings.Contains(pkg, "...") {
--- end

[an import path is read as a directory]
file scripts/mutate/testlist.go
--- anchor
	if pkg != "." && pkg != ".." && !strings.HasPrefix(pkg, "./") && !strings.HasPrefix(pkg, "../") {
--- replace
	if false {
--- end

[GOFLAGS is not left to go test -list]
file scripts/mutate/testlist.go
--- anchor
	if !ok || env.GOFLAGS != "" {
--- replace
	if !ok {
--- end

[a Test function with the wrong signature is listed instead of refused]
file scripts/mutate/testlist.go
--- anchor
		if !takesPointerTo(fn, "T") || fn.Type.TypeParams.NumFields() > 0 {
--- replace
		if fn.Type.TypeParams.NumFields() > 0 {
--- end

[a generic Test function is listed instead of refused]
file scripts/mutate/testlist.go
--- anchor
		if !takesPointerTo(fn, "T") || fn.Type.TypeParams.NumFields() > 0 {
--- replace
		if !takesPointerTo(fn, "T") {
--- end

[a dot-imported *T is not accepted]
file scripts/mutate/testlist.go
--- anchor
	case *ast.Ident:
		return x.Name == typ
--- replace
	case *ast.Ident:
		return x.Name == typ && false
--- end

[*testing.T is not accepted]
file scripts/mutate/testlist.go
--- anchor
	case *ast.SelectorExpr:
		return x.Sel.Name == typ
--- replace
	case *ast.SelectorExpr:
		return x.Sel.Name != typ
--- end

[a function with results passes the signature check]
file scripts/mutate/testlist.go
--- anchor
	if fn.Type.Results.NumFields() > 0 || len(params) != 1 || len(params[0].Names) > 1 {
--- replace
	if len(params) != 1 || len(params[0].Names) > 1 {
--- end
