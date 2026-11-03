pkg ./internal/postproc/
run TestUnpackStage_MissingFirstVolumesIsNotHandedToUnrar
tags integration

[the driver ignores noRetry]
file internal/postproc/stage_unpack.go
--- anchor
	if goErr != nil && ctx.Err() == nil && d.fallback && (d.noRetry == nil || !d.noRetry(goErr)) {
--- replace
	if goErr != nil && ctx.Err() == nil && d.fallback {
--- end

[the RAR driver retries an archive missing its first volume]
file internal/postproc/stage_unpack.go
--- anchor
		noRetry:     unpack.IsNotFirstVolume,
--- replace
		noRetry:     nil,
--- end
