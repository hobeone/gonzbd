pkg ./internal/unpack/
run ^(TestPublishStagedExtraction_ReadOnlyStagedDir|TestRemoveStage_ReadOnlyStagedDir)$

# #862: an external extractor restores the directory modes an archive stores.
# A staged directory without owner write could have no file renamed out of it,
# so publishing failed, and the stage could not be removed afterwards either.

# The stage's directories are left as the extractor made them: renaming ro/f
# out of the 0o555 ro fails, even though its destination directory is 0o750.
[publish does not make the stage owner-writable]
file internal/unpack/snapshot.go
--- anchor
	if err := ownStageDirs(stageDir); err != nil {
		return nil, err
	}
--- replace
	if err := error(nil); err != nil {
		return nil, err
	}
--- end

# Destination directories are created with 0o711 instead of 0o750, so the
# published directory's mode differs from the 0o750 the test compares it with.
[destination directories are not created with 0o750]
file internal/unpack/snapshot.go
--- anchor
	err := root.MkdirAll(rel, 0o750)
--- replace
	err := root.MkdirAll(rel, 0o711)
--- end

# Removal no longer grants the owner bits before retrying, so both
# removeStageDir and CleanupStageDirs leave the stage behind.
[removal does not retry with owner permissions]
file internal/unpack/snapshot.go
--- anchor
	if ownErr := ownStageDirs(stageDir); ownErr != nil {
--- replace
	if ownErr := error(nil); ownErr != nil {
--- end

# ownStageDirs skips every directory: the shared helper is a no-op.
[ownStageDirs changes no mode]
file internal/unpack/snapshot.go
--- anchor
	if perm := info.Mode().Perm(); perm&0o700 != 0o700 {
--- replace
	if perm := info.Mode().Perm(); false && perm&0o700 != 0o700 {
--- end

# Owner search is not granted: the 0o000 locked directory becomes 0o600 and
# cannot be walked into.
[ownStageDirs grants no owner search]
file internal/unpack/snapshot.go
--- anchor
		return root.Chmod(path, perm|0o700)
--- replace
		return root.Chmod(path, perm|0o600)
--- end

# Only a directory missing owner write is changed: noread, at 0o300, keeps
# its missing read bit and cannot be listed.
[ownStageDirs fixes only a missing write bit]
file internal/unpack/snapshot.go
--- anchor
	if perm := info.Mode().Perm(); perm&0o700 != 0o700 {
--- replace
	if perm := info.Mode().Perm(); perm&0o200 == 0 {
--- end

# CleanupStageDirs goes back to a bare os.RemoveAll.
[CleanupStageDirs does not use removeStageTree]
file internal/unpack/snapshot.go
--- anchor
			_ = removeStageTree(filepath.Join(outDir, e.Name())) //nolint:errcheck // best effort; a stage left behind is retried by every later CleanupStageDirs
--- replace
			_ = os.RemoveAll(filepath.Join(outDir, e.Name())) //nolint:errcheck // best effort; a stage left behind is retried by every later CleanupStageDirs
--- end
