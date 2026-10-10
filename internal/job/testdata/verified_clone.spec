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
			p.written[fileIdx] = sortedClone(rows)
--- replace
			p.written[fileIdx] = rows
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
		if r.FileIdx != fileIdx || int(r.ArtIdx) < lo || int(r.ArtIdx) >= hi || r.Offset < 0 || r.Length <= 0 {
--- replace
		if int(r.ArtIdx) < lo || int(r.ArtIdx) >= hi || r.Offset < 0 || r.Length <= 0 {
--- end

[a later install drops the resident rows]
file internal/job/verified.go
--- anchor
	for _, r := range resident {
		byArt[r.ArtIdx] = r
	}
--- replace
	for range resident {
	}
--- end
