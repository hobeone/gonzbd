pkg ./internal/postproc/
run ^TestStageConfig_ReturnsWhatApplyInstalled$|^TestScriptStage_SetScriptDirTakesEffect$

[unpack Config drops the cleanup flag]
file internal/postproc/stage_unpack.go
--- anchor
		Cleanup:         u.cleanup.Load(),
--- replace
		Cleanup:         false,
--- end

[unpack Config drops the base options]
file internal/postproc/stage_unpack.go
--- anchor
		Base:            u.baseOpts,
--- replace
		Base:            unpack.Options{},
--- end

[unpack Config drops the tar toggle]
file internal/postproc/stage_unpack.go
--- anchor
		EnableTar:       u.types.tar,
--- replace
		EnableTar:       false,
--- end

[repair Config drops the fallback toggle]
file internal/postproc/stage_repair.go
--- anchor
	return RepairConfig{Par2: s.par2Opts, UseGoPar2: s.useGoPar2, GoPar2Fallback: s.goPar2Fallback}
--- replace
	return RepairConfig{Par2: s.par2Opts, UseGoPar2: s.useGoPar2}
--- end

[SetScriptDir does not install the directory]
file internal/postproc/stage_script.go
--- anchor
	s.scriptDir = dir
--- replace
	_ = dir
--- end
