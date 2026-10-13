pkg ./internal/app/
run ^(TestUnpackConfigFromPP_ForwardsArchiveTypeToggles|TestUnpackStageEnabled_AnyArchiveType)$

# unpackConfigFromPP carries each postproc.enable_* archive toggle to the
# unpack stage, which skips an archive whose type is off (#947).

[enable_unrar is not forwarded]
file internal/app/reload_translate.go
--- anchor
		EnableUnrar:     pp.EnableUnrar,
--- replace
		EnableUnrar:     true,
--- end

[enable_7zip is not forwarded]
file internal/app/reload_translate.go
--- anchor
		Enable7zip:      pp.Enable7zip,
--- replace
		Enable7zip:      true,
--- end

[enable_filejoin is not forwarded]
file internal/app/reload_translate.go
--- anchor
		EnableFileJoin:  pp.EnableFileJoin,
--- replace
		EnableFileJoin:  true,
--- end

[enable_tar is not forwarded]
file internal/app/reload_translate.go
--- anchor
		EnableTar:       pp.EnableTar,
--- replace
		EnableTar:       true,
--- end

# unpackStageEnabled is the one computation of the stage-level flag that
# buildStages and ReloadPostProcOptions both set.

[the stage ignores enable_unrar]
file internal/app/reload_translate.go
--- anchor
	return pp.EnableUnrar || pp.Enable7zip || pp.EnableFileJoin || pp.EnableTar
--- replace
	return pp.Enable7zip || pp.EnableFileJoin || pp.EnableTar
--- end

[the stage ignores enable_tar]
file internal/app/reload_translate.go
--- anchor
	return pp.EnableUnrar || pp.Enable7zip || pp.EnableFileJoin || pp.EnableTar
--- replace
	return pp.EnableUnrar || pp.Enable7zip || pp.EnableFileJoin
--- end

[the stage is always on]
file internal/app/reload_translate.go
--- anchor
	return pp.EnableUnrar || pp.Enable7zip || pp.EnableFileJoin || pp.EnableTar
--- replace
	return true
--- end
