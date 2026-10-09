pkg ./internal/job/
run TestFileRows_ReturnsACopy|TestInstallVerified_NeitherKeepsNorReordersTheCallersSlice

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
