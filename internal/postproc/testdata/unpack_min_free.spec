pkg ./internal/postproc/
run ^TestUnpackStage_MinFreeSpaceReachesExtraction$

# The unpack stage hands downloads.min_free_space to every extraction, so the
# free-space guard in internal/unpack refuses what would not fit (#901).

[Run does not pass the stage's min free space]
file internal/postproc/stage_unpack.go
--- anchor
	opts.MinFreeSpace = u.minFreeSpace.Load()
--- replace
	_ = u.minFreeSpace.Load()
--- end

[SetMinFreeSpace stores nothing]
file internal/postproc/stage_unpack.go
--- anchor
func (u *UnpackStage) SetMinFreeSpace(bytes int64) { u.minFreeSpace.Store(bytes) }
--- replace
func (u *UnpackStage) SetMinFreeSpace(bytes int64) { _ = bytes }
--- end
