pkg ./internal/assembler/
run Test(WriteFault_IsRoutedOutOfTheAssembler)$

[routeOutcome drops the write-fault report]
file internal/assembler/assembler.go
--- anchor
		a.noteArticlesUnwritten(req, []int32{req.ArtIdx})
		a.noteWriteFault(f.info.Path(), req, o.fault)
--- replace
		a.noteWriteFault(f.info.Path(), req, o.fault)
--- end

[routeOutcome reports the write fault twice]
file internal/assembler/assembler.go
--- anchor
		a.noteArticlesUnwritten(req, []int32{req.ArtIdx})
		a.noteWriteFault(f.info.Path(), req, o.fault)
--- replace
		a.noteArticlesUnwritten(req, []int32{req.ArtIdx})
		a.noteArticlesUnwritten(req, []int32{req.ArtIdx})
		a.noteWriteFault(f.info.Path(), req, o.fault)
--- end
