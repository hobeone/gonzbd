pkg ./internal/assembler/
run TestAssembler_HelperMethods/handleFatalArticle

[handleFatalArticle stops releasing req.Data on every return]
file internal/assembler/assembler.go
--- anchor
	admitted := f.w.admitPermanentFailure(req.ArtIdx)
	if req.Data != nil {
		a.releaseBuffer(req.Data)
	}
	return admitted
--- replace
	admitted := f.w.admitPermanentFailure(req.ArtIdx)
	return admitted
--- end

[handleFatalArticle regresses to gating the release on admitted, as processRequest used to]
file internal/assembler/assembler.go
--- anchor
	admitted := f.w.admitPermanentFailure(req.ArtIdx)
	if req.Data != nil {
		a.releaseBuffer(req.Data)
	}
	return admitted
--- replace
	admitted := f.w.admitPermanentFailure(req.ArtIdx)
	if admitted && req.Data != nil {
		a.releaseBuffer(req.Data)
	}
	return admitted
--- end
