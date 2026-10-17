pkg ./internal/nzb/
run TestExtractFilenameFromSubject$|TestParseHexNamedPar2Index

[the hex-name fallback dropped, so the unquoted search or the subject wins]
file internal/nzb/subject.go
--- anchor
	if hexName != "" {
		return hexName
	}
--- replace
	if false {
		return hexName
	}
--- end

[a hex candidate accepted as soon as it is seen]
file internal/nzb/subject.go
--- anchor
		if !isExcessivelyObfuscated(name) {
			return true
		}
--- replace
		if true {
			return true
		}
--- end

[the last hex candidate kept instead of the first]
file internal/nzb/subject.go
--- anchor
		if hexName == "" {
			hexName = name
		}
--- replace
		if true {
			hexName = name
		}
--- end

[an unquoted hex match refused, as before the fix]
file internal/nzb/subject.go
--- anchor
	if name := strings.TrimSpace(reSubjectBasicFilename.FindString(subject)); name != "" {
--- replace
	if name := strings.TrimSpace(reSubjectBasicFilename.FindString(subject)); name != "" && !isExcessivelyObfuscated(name) {
--- end

[the PRiVATE step's hex candidate not recorded]
file internal/nzb/subject.go
--- anchor
	if name := parsePRiVATESubject(subject); pick(name) {
--- replace
	if name := parsePRiVATESubject(subject); name != "" && !isExcessivelyObfuscated(name) {
--- end
