pkg ./internal/assembler/
run ^(TestWriteArticle_RefusesARefWithoutAnOwner|TestOpenTargetFile_ResolvesWithTheRefsOwner)$

# The ref carries its owner and the worker derives the job ID from it
# (#1056). A nil owner is refused at WriteArticle; the resolver receives the
# owner the ref carried.

[the nil-owner check is gone]
file internal/assembler/assembler.go
--- anchor
	if ref.Owner == nil {
--- replace
	if false {
--- end

[the ID is not derived from the owner]
file internal/assembler/assembler.go
--- anchor
	req.JobID = ref.Owner.ID()
--- replace
	req.JobID = ""
--- end
