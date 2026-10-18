pkg ./internal/app/
run ^(TestPar2Verdict_UnreadableSetIsNotACleanVerdict|TestRecoveryDeficit)$

# A par2 set on disk that reads as nothing keeps the verdict off clean, and
# keeps the release unsized (#1000).

[the verdict ignores an unreadable set]
file internal/app/app.go
--- anchor
		if len(id.Unreadable) > 0 {
--- replace
		if false {
--- end

[the sizing ignores an unreadable set]
file internal/app/par2_release.go
--- anchor
	if len(a.ID.Unreadable) > 0 {
--- replace
	if false {
--- end
