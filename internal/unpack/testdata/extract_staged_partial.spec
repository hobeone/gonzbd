pkg ./internal/unpack/
run ^TestExtractStaged_APartialPublish

[the undo no longer removes the files it published]
file internal/unpack/snapshot.go
--- anchor
	for _, p := range added {
--- replace
	for _, p := range added[:0] {
--- end

[the undo no longer removes the directories it created]
file internal/unpack/snapshot.go
--- anchor
		if err := root.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
--- replace
		_ = dir
		if err := fs.ErrNotExist; err != nil && !errors.Is(err, fs.ErrNotExist) {
--- end

[the undo removes a replaced name too]
file internal/unpack/snapshot.go
--- anchor
		free := errors.Is(lerr, fs.ErrNotExist)
--- replace
		free := errors.Is(lerr, fs.ErrNotExist) || lerr == nil
--- end
[every level is recorded as created whether or not it existed before]
file internal/unpack/snapshot.go
--- anchor
		if _, err := root.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			break
		}
--- replace
		if _, err := root.Lstat(dir); !errors.Is(err, fs.ErrNotExist) && false {
			break
		}
--- end
