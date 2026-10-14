pkg ./internal/unpack/
run TestExtractStaged_APartialPublishLeavesNothing

[the undo no longer removes the files it published]
file internal/unpack/snapshot.go
--- anchor
		errs = append(errs, fsutil.RemoveRootAll(root, p, filepath.Join(outDir, p)))
--- replace
		if false {
			errs = append(errs, fsutil.RemoveRootAll(root, p, filepath.Join(outDir, p)))
		}
--- end

[the undo no longer removes the directories it created]
file internal/unpack/snapshot.go
--- anchor
		if err := root.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
--- replace
		_ = dir
		if err := fs.ErrNotExist; err != nil && !errors.Is(err, fs.ErrNotExist) {
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
