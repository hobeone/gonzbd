pkg ./internal/deobfuscate/
run TestPar2Rename_SkipsPar2NumberedBackupsWhenTargetExists

[skip par2 backup check disabled in par2RenameFile]
file internal/deobfuscate/par2.go
--- anchor
		if isPar2BackupOf(e.Name(), trueName, relDst) {
--- replace
		if false && isPar2BackupOf(e.Name(), trueName, relDst) {
--- end

[hasPar2BackupSuffix dot check dropped]
file internal/deobfuscate/par2.go
--- anchor
	if !strings.EqualFold(name[:len(base)], base) || name[len(base)] != '.' {
--- replace
	if !strings.EqualFold(name[:len(base)], base) {
--- end

[isPar2BackupNumber leading zero allowed]
file internal/deobfuscate/par2.go
--- anchor
	if suffix == "" || len(suffix) > 3 || suffix[0] < '1' || suffix[0] > '9' {
--- replace
	if suffix == "" || len(suffix) > 3 || suffix[0] < '0' || suffix[0] > '9' {
--- end

[isPar2BackupNumber max length 3 relaxed]
file internal/deobfuscate/par2.go
--- anchor
	if suffix == "" || len(suffix) > 3 || suffix[0] < '1' || suffix[0] > '9' {
--- replace
	if suffix == "" || len(suffix) > 4 || suffix[0] < '1' || suffix[0] > '9' {
--- end
