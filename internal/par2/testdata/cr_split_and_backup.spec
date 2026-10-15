pkg ./internal/par2/
run TestParseRepairOutput_CRDelimitedProgressLines|TestIsBackupNamedFor

[ParseRepairOutput splits on LF only instead of CR and LF]
file internal/par2/parse_output.go
--- anchor
	for rawLine := range strings.FieldsFuncSeq(output, func(r rune) bool { return r == '\n' || r == '\r' }) {
--- replace
	for rawLine := range strings.FieldsFuncSeq(output, func(r rune) bool { return r == '\n' }) {
--- end

[IsBackupNumber accepts zero-padded split suffixes]
file internal/par2/backup.go
--- anchor
	if suffix == "" || len(suffix) > 3 || suffix[0] < '1' || suffix[0] > '9' {
--- replace
	if suffix == "" || len(suffix) > 3 || suffix[0] < '0' || suffix[0] > '9' {
--- end

[IsBackupNamedFor skips target hash verification]
file internal/par2/backup.go
--- anchor
	return err == nil && targetHash == wantHash
--- replace
	return err == nil && (targetHash == wantHash || true)
--- end
