pkg ./internal/config/
run ^(TestExtraArgs|TestValidate)

[the unrar allowlist is not applied]
file internal/config/postproc.go
--- anchor
		err = cmdutil.ValidateUnrarParams(args)
--- replace
		_ = args
--- end

[a refused unrar argv comes back alongside its error]
file internal/config/postproc.go
--- anchor
		return nil, fmt.Errorf("extra_unrar_params: %w", err)
--- replace
		return args, fmt.Errorf("extra_unrar_params: %w", err)
--- end

[Validate does not ask for the par2 argv]
file internal/config/validate.go
--- anchor
	if _, err := p.Par2ExtraArgs(); err != nil {
--- replace
	if _, err := p.Par2ExtraArgs(); false && err != nil {
--- end
