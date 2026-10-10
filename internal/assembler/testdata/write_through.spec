pkg ./internal/assembler/
run Test(WriteFault_IsRoutedOutOfTheAssembler)$

[routeAcceptFailure drops the write-fault report]
file internal/assembler/assembler.go
--- anchor
	a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
	a.noteWriteFault(f.info.Path(), req, err)
--- replace
	a.noteWriteFault(f.info.Path(), req, err)
--- end

[routeAcceptFailure reports the write fault twice]
file internal/assembler/assembler.go
--- anchor
	a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
	a.noteWriteFault(f.info.Path(), req, err)
--- replace
	a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
	a.noteArticlesUnwritten(req.JobID, req.FileIdx, []int32{req.ArtIdx})
	a.noteWriteFault(f.info.Path(), req, err)
--- end
