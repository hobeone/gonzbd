pkg ./internal/job/
run TestFileRows_ReturnsACopy|TestInstallVerified_NeitherKeepsNorReordersTheCallersSlice

[a Progress() clone shares the live row map]
file internal/job/progress.go
--- anchor
	cp.written = maps.Clone(p.written)
--- replace
	cp.written = p.written
--- end

[the sorted copy is sorted in place]
file internal/job/verified.go
--- anchor
	out := slices.Clone(rows)
--- replace
	out := rows
--- end
