pkg ./internal/app/
run TestWriteArticle_AfterTheJobClosedCreatesNoFile$

# #1053's acceptance: a write that reaches the assembler worker after the job
# left the dispatcher or was handed off creates no file. The resolver's two
# questions, asked of the owner the write carries, are what stand in its way,
# whether or not the cache still holds the file (a RemoveJob's or
# enqueuePostProc's forgetJob may have dropped it).

[the resolver ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	if !p.holds(j) {
		return assembler.FileInfo{}, fmt.Errorf("resolve %s[%d]: instance left the dispatcher: %w", j.ID(), fileIdx, assembler.ErrJobClosed)
--- replace
	if false {
		return assembler.FileInfo{}, fmt.Errorf("resolve %s[%d]: instance left the dispatcher: %w", j.ID(), fileIdx, assembler.ErrJobClosed)
--- end

[the resolver ignores the hand-over]
file internal/app/pipeline.go
--- anchor
	if p.handedOff != nil && p.handedOff(j) {
--- replace
	if false {
--- end
