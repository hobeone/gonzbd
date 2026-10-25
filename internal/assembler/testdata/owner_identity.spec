pkg ./internal/assembler/
run ^(TestWriteArticle_RefusesARefWithoutAnOwner|TestOpenTargetFile_ResolvesWithTheRefsOwner|TestProcessRequest_RefusesAnotherOwnersWriteOnAnOpenHandle|TestProcessRequest_EvictsADepartedOwnersHandle)$

# The ref carries its owner and the worker derives the job ID from it
# (#1056). A nil owner is refused at WriteArticle; the resolver receives the
# owner the ref carried; an open handle serves only the owner that opened
# it, and a departed owner's handle is evicted for the held one.
#
# Not mutated: the closeCancelledFile call in the eviction branch. Skipping
# it leaks the predecessor's descriptor and changes no bytes any test here
# can read, so no assertion discriminates it.

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

[an open handle serves every owner]
file internal/assembler/assembler.go
--- anchor
	if ok && f.owner != req.Owner {
--- replace
	if false {
--- end

[the departed owner's handle is kept and reused]
file internal/assembler/assembler.go
--- anchor
		a.closeCancelledFile(f, KeepFiles)
		delete(open, key)
		ok = false
--- replace
		a.closeCancelledFile(f, KeepFiles)
		delete(open, key)
--- end
