pkg ./internal/app/
run TestWriteArticle_OfAnEarlierAttemptNeverReachesTheRetrysFile$

# #1056's acceptance: a write of an earlier attempt that reaches the worker
# after the retry registered and opened the same file index lands nowhere.
# Two things stand in its way: the assembler's fast path refuses a handle to
# another owner, and the resolver asks the dispatcher about the owner the
# write carries.

[the resolver ignores the dispatcher]
file internal/app/pipeline.go
--- anchor
	if !p.holds(j) {
		return assembler.FileInfo{}, fmt.Errorf("resolve %s[%d]: instance left the dispatcher: %w", j.ID(), fileIdx, assembler.ErrJobClosed)
--- replace
	if false {
		return assembler.FileInfo{}, fmt.Errorf("resolve %s[%d]: instance left the dispatcher: %w", j.ID(), fileIdx, assembler.ErrJobClosed)
--- end

[an open handle serves every owner]
file internal/assembler/assembler.go
--- anchor
	if ok && f.owner != req.Owner {
--- replace
	if false {
--- end
