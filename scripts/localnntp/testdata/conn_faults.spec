pkg ./scripts/localnntp/
run TestServer_KillActive|TestServer_AuthFail|TestServer_UpdateConnSettings|TestControl_Conn|TestCLI_Serve

[KillActive leaves killed connections in the open set]
run TestServer_KillActive_FreesSlotsImmediately|TestServer_KillActive$
file scripts/localnntp/server.go
--- anchor
		_ = c.Close() //nolint:errcheck // abrupt close is the point
		delete(s.openConns, c)
--- replace
		_ = c.Close() //nolint:errcheck // abrupt close is the point
--- end

[admit runs on the connection goroutine instead of the accept loop]
run TestServer_KillActive_CoversJustAcceptedConnection
file scripts/localnntp/server.go
--- anchor
		greeting, admitted := s.admit(c)
		s.wg.Go(func() {
			s.serveConn(c, greeting, admitted)
		})
--- replace
		s.wg.Go(func() {
			greeting, admitted := s.admit(c)
			s.serveConn(c, greeting, admitted)
		})
--- end

[auth_fail spares connections that already authenticated]
run TestServer_AuthFail_AppliesToAuthenticatedConnections
file scripts/localnntp/server.go
--- anchor
	return s.authFail.Load() || (len(s.cfg.Users) > 0 && !cs.authed)
--- replace
	return (s.authFail.Load() && !cs.authed) || (len(s.cfg.Users) > 0 && !cs.authed)
--- end

[UpdateConnSettings reads, mutates and stores in three separate critical sections]
run TestServer_UpdateConnSettings_IsAtomic|TestControl_Conn_ConcurrentPartialUpdates
file scripts/localnntp/server.go
--- anchor
	s.connMu.Lock()
	defer s.connMu.Unlock()
	next := s.connSettings
	mutate(&next)
--- replace
	s.connMu.Lock()
	next := s.connSettings
	s.connMu.Unlock()
	mutate(&next)
	time.Sleep(time.Millisecond)
	s.connMu.Lock()
	defer s.connMu.Unlock()
--- end

[a negative max_connections passes validation]
run TestServer_UpdateConnSettings_RejectsNegativeCap|TestControl_Conn$|TestCLI_Serve_RejectsNegativeMaxConnections
file scripts/localnntp/server.go
--- anchor
	if c.MaxConnections < 0 {
--- replace
	if false {
--- end

[NewServer skips validating the initial connection settings]
run TestServer_UpdateConnSettings_RejectsNegativeCap|TestCLI_Serve_RejectsNegativeMaxConnections
file scripts/localnntp/server.go
--- anchor
	if err := cfg.Conn.validate(); err != nil {
		return nil, err
	}
--- replace
	if false {
		return nil, nil
	}
--- end
