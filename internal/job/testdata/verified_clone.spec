pkg ./internal/job/
run TestFileRows_ReturnsACopy|TestInstallRows_KeepsACopyOfTheFirstInstall|TestInstallVerified_NeitherKeepsNorReordersTheCallersSlice|TestInstallVerified_ARowItCannotPlaceCostsOnlyItself|TestInstallVerified_MergesALaterInstallWithTheResidentRows

[a Progress() clone shares the live row map]
file internal/job/progress.go
--- anchor
	cp.written = maps.Clone(p.written)
--- replace
	cp.written = p.written
--- end

[the first install keeps the caller's slice]
file internal/job/verified.go
--- anchor
	out := p.written[fileIdx]
--- replace
	out := p.written[fileIdx]
	if len(out) == 0 {
		p.written[fileIdx] = rows
		return
	}
--- end

[the sorted copy is sorted in place]
file internal/job/verified.go
--- anchor
	out := slices.Clone(rows)
--- replace
	out := rows
--- end

[the file check dropped from the placement guard]
file internal/job/verified.go
--- anchor
		if r.FileIdx != fileIdx || !m.ArticleInFile(r.FileIdx, r.ArtIdx) || r.Offset < 0 || r.Length <= 0 {
--- replace
		if !m.ArticleInFile(fileIdx, r.ArtIdx) || r.Offset < 0 || r.Length <= 0 {
--- end

[a later install drops the resident rows]
file internal/job/verified.go
--- anchor
	for k, r := range out {
		at[r.ArtIdx] = k
	}
--- replace
	out = nil
--- end
