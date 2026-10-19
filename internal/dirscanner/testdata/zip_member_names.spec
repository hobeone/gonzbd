# Red check for naming zip-contained NZBs after the member (#1017).
#
#     go run ./scripts/mutate internal/dirscanner/testdata/zip_member_names.spec

pkg ./internal/dirscanner/
run TestHandleStableFile_ZipMembersNamedAfterMembers|TestMemberBaseName_NeverCarriesADirectory

[the scanner names a member after the archive again]
file internal/dirscanner/scanner.go
--- anchor
		if p.Name != "" {
--- replace
		if false {
--- end

[extractZip drops the member name]
file internal/dirscanner/decompress.go
--- anchor
Payload{Name: memberBaseName(file.Name), Data: data}
--- replace
Payload{Data: data}
--- end

[control characters are accepted in a name]
file internal/dirscanner/decompress.go
--- anchor
		if unicode.IsControl(r) {
--- replace
		if unicode.IsControl(r) && r > unicode.MaxRune {
--- end

[an over-long name is accepted]
file internal/dirscanner/decompress.go
--- anchor
 || len(base) > maxMemberNameLen {
--- replace
 {
--- end

[the member name keeps its directory]
file internal/dirscanner/decompress.go
--- anchor
	base := name[strings.LastIndex(name, "/")+1:]
--- replace
	base := name
--- end
