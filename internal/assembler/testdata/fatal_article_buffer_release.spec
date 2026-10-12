pkg ./internal/assembler/
run TestAssembler_HelperMethods/handleFatalArticle

[handleFatalArticle stops releasing req.Data on every return]
file internal/assembler/assembler.go
--- anchor
	f.w.resolve(req.ArtIdx, dispositionFetchFailed)
	if req.Data != nil {
		a.releaseBuffer(req.Data)
	}
--- replace
	f.w.resolve(req.ArtIdx, dispositionFetchFailed)
--- end

[handleFatalArticle regresses to gating the release on a counted part, as processRequest used to]
file internal/assembler/assembler.go
--- anchor
	f.w.resolve(req.ArtIdx, dispositionFetchFailed)
	if req.Data != nil {
		a.releaseBuffer(req.Data)
	}
--- replace
	before := f.w.parts()
	f.w.resolve(req.ArtIdx, dispositionFetchFailed)
	if f.w.parts() > before && req.Data != nil {
		a.releaseBuffer(req.Data)
	}
--- end
