pkg ./internal/app/
run TestWriteArticle_AfterTheJobClosedCreatesNoFile$

# #1053's acceptance: a write that reaches the assembler worker after the job
# left the dispatcher or was handed off creates no file. The resolver's two
# checks are what stand in its way.

[the resolver ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	if !p.holds(entry.j) {
--- replace
	if false {
--- end

[the resolver ignores the hand-over]
file internal/app/pipeline.go
--- anchor
	if p.handedOff != nil && p.handedOff(entry.j) {
--- replace
	if false {
--- end
