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

# The destination copies the staged directory's mode again. ownStageDirs has
# already run, so the rename still succeeds; the test sees ro published with
# 0o755 (0o555 plus the owner bits) instead of 0o750. Under a umask that
# clears 0o027 both modes collapse to 0o750 and this mutation survives.
[publish copies the staged directory's mode]
file internal/unpack/snapshot.go
--- anchor
			return root.MkdirAll(rel, 0o750)
--- replace
			info, err := d.Info()
			if err != nil {
				return err
			}
			return root.MkdirAll(rel, info.Mode().Perm())
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

# CleanupStageDirs goes back to a bare os.RemoveAll.
[CleanupStageDirs does not use removeStageTree]
file internal/unpack/snapshot.go
--- anchor
			_ = removeStageTree(filepath.Join(outDir, e.Name())) //nolint:errcheck // best effort; a stage left behind is retried by every later CleanupStageDirs
--- replace
			_ = os.RemoveAll(filepath.Join(outDir, e.Name())) //nolint:errcheck // best effort; a stage left behind is retried by every later CleanupStageDirs
--- end
