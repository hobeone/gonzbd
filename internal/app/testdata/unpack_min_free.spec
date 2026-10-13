pkg ./internal/app/
run ^TestMinFreeSpace_ReachesUnpackStage$

# downloads.min_free_space reaches the unpack stage at construction and on
# every change (#901).

[buildStages does not pass min_free_space]
file internal/app/stages.go
--- anchor
	unpackStage.SetMinFreeSpace(int64(snap.Downloads.MinFreeSpace))
--- replace
	_ = snap.Downloads.MinFreeSpace
--- end

[SetMinFreeSpace does not reach the unpack stage]
file internal/app/reloader.go
--- anchor
		app.stages.Unpack.SetMinFreeSpace(bytes)
--- replace
		_ = bytes
--- end
