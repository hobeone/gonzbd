pkg ./internal/api/
run ^TestConfigTestServer_RequestParameters$

[test_server dials the request without config's check]
file internal/api/config.go
--- anchor
	return cfg.Checked()
--- replace
	if cfg.Port == 0 {
		cfg.Port = 119
	}
	return cfg, nil
--- end

[a non-numeric port is replaced by the default]
file internal/api/config.go
--- anchor
		port, err := strconv.Atoi(v)
		if err != nil {
--- replace
		port, err := strconv.Atoi(v)
		if err != nil && false {
--- end

[a non-numeric ssl_verify is replaced by the default]
file internal/api/config.go
--- anchor
		level, err := strconv.ParseInt(v, 10, 8)
		if err != nil {
--- replace
		level, err := strconv.ParseInt(v, 10, 8)
		if err != nil && false {
--- end

[an explicit 119 with SSL is rewritten to 563]
file internal/api/config.go
--- anchor
		cfg.Port = port
--- replace
		cfg.Port = port
		if cfg.SSL && port == 119 {
			cfg.Port = 563
		}
--- end
