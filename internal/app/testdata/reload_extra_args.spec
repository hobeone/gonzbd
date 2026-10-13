pkg ./internal/app/
run ^(TestApplication_ReloadPostProcOptions_RefusedArgsNotApplied|TestBuildStages_BadExtraUnrarParams|TestBuildStages_DisallowedExtraUnrarParams)$

[reload warns about an unrar flag off the allowlist and still applies it]
file internal/app/reloader.go
--- anchor
	extraUnrarArgs, err := pp.UnrarExtraArgs()
--- replace
	extraUnrarArgs, err := cmdutil.ParseExtraParams(pp.ExtraUnrarParams)
--- end

[a refused par2 argv comes back alongside its error and reload applies it]
file internal/config/postproc.go
--- anchor
		return nil, fmt.Errorf("extra_par2_params: %w", err)
--- replace
		return []string{p.ExtraPar2Params}, fmt.Errorf("extra_par2_params: %w", err)
--- end

[startup ignores a refused unrar argv]
file internal/app/stages.go
--- anchor
	extraUnrarArgs, err := pp.UnrarExtraArgs()
	if err != nil {
--- replace
	extraUnrarArgs, err := pp.UnrarExtraArgs()
	if err != nil && false {
--- end
