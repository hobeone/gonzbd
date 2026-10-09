pkg ./internal/job/
run TestFileRows_ReturnsACopy

[a Progress() clone shares each file's row slice]
file internal/job/progress.go
--- anchor
			cp.written[fi] = slices.Clone(rows)
--- replace
			cp.written[fi] = rows
--- end
