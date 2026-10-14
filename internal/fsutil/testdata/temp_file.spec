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

[IsTempFile skips par2engine temp-file check]
file internal/fsutil/rootedcreate.go
--- anchor
	return isGonzbdTempFile(name) || isPar2EngineTempFile(name)
--- replace
	return isGonzbdTempFile(name)
--- end

[isPar2EngineTempFile accepts empty target prefix]
file internal/fsutil/rootedcreate.go
--- anchor
	if len(name) <= par2TempSuffix || !strings.HasSuffix(name, par2TempExt) {
--- replace
	if len(name) < par2TempSuffix || !strings.HasSuffix(name, par2TempExt) {
--- end

[isPar2EngineTempFile skips marker check]
file internal/fsutil/rootedcreate.go
--- anchor
	if name[markerStart:markerStart+len(par2TempMarker)] != par2TempMarker {
--- replace
	if false && name[markerStart:markerStart+len(par2TempMarker)] != par2TempMarker {
--- end

[isPar2EngineTempFile skips base32 character validation]
file internal/fsutil/rootedcreate.go
--- anchor
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
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
