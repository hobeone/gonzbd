pkg ./internal/fsutil/
run TestCopyAndRemoveWithin_SiblingDirectoryLinkStaysInsideRoot|TestCopyAndRemoveWithin_LinkLeavingRootStillRefused

[a symlink is checked against its own directory instead of the job root]
file internal/fsutil/move.go
--- anchor
	resolved = filepath.Clean(resolved)

	absRoot, err := filepath.Abs(root)
--- replace
	resolved = filepath.Clean(resolved)

	absRoot, err := filepath.Abs(filepath.Dir(symlinkPath))
--- end
