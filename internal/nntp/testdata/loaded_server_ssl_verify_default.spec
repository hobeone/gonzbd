pkg ./internal/nntp/
run ^TestBuildTLSConfig_LoadedServerWithoutSSLVerifyVerifies$

[a loaded server that omits ssl_verify gets the zero value]
file internal/config/servers.go
--- anchor
	return ServerConfig{SSLVerify: DefaultSSLVerify}
--- replace
	return ServerConfig{}
--- end

[Load keeps the strict pass's zero-filled servers]
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
