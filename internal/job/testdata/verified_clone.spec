pkg ./internal/job/
run TestFileRows_ReturnsACopy|TestInstallVerified_NeitherKeepsNorReordersTheCallersSlice|TestInstallVerified_RefusesARowItCannotPlace|TestInstallVerified_MergesALaterInstallWithTheResidentRows

[a Progress() clone shares each file's row slice]
file internal/job/progress.go
--- anchor
			cp.written[fi] = slices.Clone(rows)
--- replace
			cp.written[fi] = rows
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
		if r.FileIdx != fileIdx || int(r.ArtIdx) < lo || int(r.ArtIdx) >= hi {
--- replace
		if int(r.ArtIdx) < lo || int(r.ArtIdx) >= hi {
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
