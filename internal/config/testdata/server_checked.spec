pkg ./internal/config/
run ^(TestServerConfig_Checked|TestDecode_ServerPortDefaultsPlain|TestDecode_ServerPortDefaultsSSL)$

[Checked does not normalize]
file internal/config/servers.go
--- anchor
	s.normalize()
	if err := s.validate(); err != nil {
--- replace
	if err := s.validate(); err != nil {
--- end

[Checked does not validate]
file internal/config/servers.go
--- anchor
	if err := s.validate(); err != nil {
		return ServerConfig{}, err
--- replace
	if err := s.validate(); false && err != nil {
		return ServerConfig{}, err
--- end

[the SSL port default is swapped]
file internal/config/servers.go
--- anchor
		if s.SSL {
			s.Port = 563
--- replace
		if !s.SSL {
			s.Port = 563
--- end

[Load does not normalize its servers]
file internal/config/loader.go
--- anchor
		cfg.Servers[i].normalize()
--- replace
		_ = i
--- end
