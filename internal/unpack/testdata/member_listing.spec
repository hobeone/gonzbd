pkg ./internal/unpack/
run TestRarMemberBaseNames

[rarMemberBaseNames ends the listing at a member refused by name]
file internal/unpack/members.go
--- anchor
		if fh == nil {
--- replace
		if err != nil {
--- end
