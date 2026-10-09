pkg ./internal/fsutil/
run TestIsTempFile|TestRootedCreateTempPerm

[IsTempFile skips prefix check]
file internal/fsutil/rootedcreate.go
--- anchor
	if !strings.HasPrefix(name, TempFilePrefix) {
--- replace
	if false && !strings.HasPrefix(name, TempFilePrefix) {
--- end

[IsTempFile accepts non-16-byte suffix]
file internal/fsutil/rootedcreate.go
--- anchor
	if len(suffix) != 16 {
--- replace
	if len(suffix) < 16 {
--- end

[IsTempFile skips lowercase hex character validation]
file internal/fsutil/rootedcreate.go
--- anchor
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
--- replace
		if false && c == 0 {
--- end

[RootedCreateTempPerm ignores perm and uses 0o600]
file internal/fsutil/rootedcreate.go
--- anchor
		f, err := root.OpenFile(tmpRel, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
--- replace
		f, err := root.OpenFile(tmpRel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
--- end
