# Red check for naming downloaded zip-contained NZBs after the member (#1017).
#
#     go run ./scripts/mutate internal/urlgrabber/testdata/zip_member_names.spec

pkg ./internal/urlgrabber/
run TestFetchZipMembersNamedAfterMembers

[the grabber names a member after the download again]
file internal/urlgrabber/grabber.go
--- anchor
		if p.Name != "" {
--- replace
		if false {
--- end
