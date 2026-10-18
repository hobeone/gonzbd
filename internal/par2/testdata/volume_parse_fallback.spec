pkg ./internal/par2/
run ^TestFindPar2Files_ReadsAVolumeWhenTheIndexDescribesNothing$

# A set whose main file describes nothing is parsed from a volume, the main
# file is preferred when it is readable, and a set with no readable file still
# names its main file (#1006).

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
