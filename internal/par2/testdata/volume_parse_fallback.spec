pkg ./internal/par2/
run ^(TestFindPar2Files_ReadsAVolumeWhenTheIndexDescribesNothing|TestParsePar2Set_KeepsEachFileOnce|TestParsePar2Set_UntilDescribedStopsAtTheFirstFileDescription|TestSet_ResolveReadable)$

# A set whose main file describes nothing is parsed from a volume, the main
# file is preferred when it is readable, and a set with no readable file still
# names its main file (#1006). A file with a File Description is preferred to
# one with only a Main packet, which is still read when nothing better exists,
# and a volume's repeated File Descriptions are kept once.

[FindPar2Files does not resolve the parse file]
file internal/par2/par2.go
--- anchor
		s.resolveReadable(parseOpts)
--- replace
		_ = parseOpts
--- end

[ParseFile ignores the resolved file]
file internal/par2/par2.go
--- anchor
	if s.readable != "" {
--- replace
	if false {
--- end

[a volume is preferred to a readable main file]
file internal/par2/par2.go
--- anchor
		candidates = append([]string{s.MainFile}, s.ExtraFiles...)
--- replace
		candidates = append(slices.Clone(s.ExtraFiles), s.MainFile)
--- end

[a file that describes nothing is accepted]
file internal/par2/par2.go
--- anchor
	return len(p.Files) > 0 || len(p.RecoveryFileIDs) > 0
--- replace
	return true
--- end

[a Main-only file is preferred to a volume that describes the files]
file internal/par2/par2.go
--- anchor
		case len(parsed.Files) > 0:
--- replace
		case describesAFile(parsed):
--- end

[a Main-only file is not read when nothing better exists]
file internal/par2/par2.go
--- anchor
	s.readable = mainOnly
--- replace
	s.readable = ""
--- end

[a repeated File Description is kept again]
file internal/par2/parser.go
--- anchor
		if fd == nil || seen.descs[fd.FileID] {
--- replace
		if fd == nil {
--- end

[the probe reads past the first File Description]
file internal/par2/parser.go
--- anchor
		if untilDescribed && len(set.Files) > 0 {
--- replace
		if false {
--- end
