pkg ./internal/config/
run ^(TestDecode_ServerSSLVerifyOmittedDefaultsToHostname|TestDecode_ServerSSLVerifyExplicitZeroKept|TestSet_ServersSSLVerifyOmittedDefaultsToHostname)$

[the default an omitted key gets is the zero value]
file internal/config/servers.go
--- anchor
	return ServerConfig{SSLVerify: DefaultSSLVerify}
--- replace
	return ServerConfig{}
--- end

[decode keeps the strict pass's zero-filled servers]
file internal/config/loader.go
--- anchor
	if servers != nil {
		cfg.Servers = servers
	}
--- replace
	if false {
		cfg.Servers = servers
	}
--- end

[decodeServers starts each YAML entry from the zero value]
file internal/config/servers.go
--- anchor
		servers[i] = DefaultServerConfig()
--- replace
		servers[i] = ServerConfig{}
--- end

[UnmarshalJSON starts each set_config entry from the zero value]
file internal/config/servers.go
--- anchor
	p := plain(DefaultServerConfig())
--- replace
	p := plain{}
--- end

[a JSON explicit 0 is rewritten to the default]
file internal/config/servers.go
--- anchor
	*s = ServerConfig(p)
--- replace
	if p.SSLVerify == SSLVerifyNone {
		p.SSLVerify = DefaultSSLVerify
	}
	*s = ServerConfig(p)
--- end

[a YAML explicit 0 is rewritten to the default]
file internal/config/servers.go
--- anchor
	return servers, nil
--- replace
	for i := range servers {
		if servers[i].SSLVerify == SSLVerifyNone {
			servers[i].SSLVerify = DefaultSSLVerify
		}
	}
	return servers, nil
--- end
