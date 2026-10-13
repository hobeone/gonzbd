pkg ./internal/config/
run ^(TestServer_ZeroValuePanics|TestServerConfig_Checked|TestConfig_CheckedServers)$

[a Server Checked did not produce can be read]
file internal/config/servers.go
--- anchor
	if !s.checked {
--- replace
	if false {
--- end

[Checked hands back a Server marked unchecked]
file internal/config/servers.go
--- anchor
	return Server{cfg: s, checked: true}, nil
--- replace
	return Server{cfg: s}, nil
--- end

[CheckedServers accepts an entry Checked refused]
file internal/config/servers.go
--- anchor
		s, err := sc.Checked()
		if err != nil {
--- replace
		s, err := sc.Checked()
		if err != nil && false {
--- end
