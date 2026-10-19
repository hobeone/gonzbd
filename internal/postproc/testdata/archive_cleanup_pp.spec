pkg ./internal/postproc/
run TestUnpackStage_CleanupArchives_RequiresDeleteLevel

[the PP = +Delete requirement neutered to always true]
file internal/postproc/stage_unpack.go
--- anchor
	if u.cleanup.Load() && job.PP >= types.PPDelete && len(allSuccessful) > 0 {
--- replace
	if u.cleanup.Load() && (job.PP >= types.PPDelete || true) && len(allSuccessful) > 0 {
--- end

[the requirement lowered to the unpack level]
file internal/postproc/stage_unpack.go
--- anchor
	if u.cleanup.Load() && job.PP >= types.PPDelete && len(allSuccessful) > 0 {
--- replace
	if u.cleanup.Load() && job.PP >= types.PPUnpack && len(allSuccessful) > 0 {
--- end
