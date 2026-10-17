pkg ./internal/downloader/
run ^(TestDownloader_StalledArticleDoesNotBenchServer|TestDownloader_ServerLosingEveryConnectionIsPenalized|TestFetchArticle_ConnFaultRunAppliesClampedPenalty|TestFetchArticle_ServerResponseFaultPenalizesAtOnce|TestServer_RecordConnFault|TestIsConnFault)$

# A connection fault mid-fetch costs its connection, not its server, until
# more of the server's connections faulted than it has, with no success in
# between (#986).

[every fetch failure is charged as a server fault, as before #986]
file internal/downloader/dispatch.go
--- anchor
	if isConnFault(err) {
		if !srv.RecordConnFault() {
--- replace
	if false {
		if !srv.RecordConnFault() {
--- end

[every fetch failure is charged as a connection fault]
file internal/downloader/dispatch.go
--- anchor
	if isConnFault(err) {
		if !srv.RecordConnFault() {
--- replace
	if true {
		if !srv.RecordConnFault() {
--- end

[a run of connection faults never earns a penalty]
file internal/downloader/server.go
--- anchor
	return n > int64(s.cfg.Connections) && s.badConns.CompareAndSwap(n, 0)
--- replace
	return false && n > int64(s.cfg.Connections) && s.badConns.CompareAndSwap(n, 0)
--- end

[a reported run does not reset the count]
file internal/downloader/server.go
--- anchor
	return n > int64(s.cfg.Connections) && s.badConns.CompareAndSwap(n, 0)
--- replace
	return n > int64(s.cfg.Connections)
--- end

[a single connection fault earns a penalty]
file internal/downloader/server.go
--- anchor
	return n > int64(s.cfg.Connections) && s.badConns.CompareAndSwap(n, 0)
--- replace
	return n > 0 && s.badConns.CompareAndSwap(n, 0)
--- end

[a timeout is not a connection fault]
file internal/downloader/penalty.go
--- anchor
		errors.Is(err, syscall.EPIPE) || isTimeout(err)
--- replace
		errors.Is(err, syscall.EPIPE)
--- end

[a lost transport is not a connection fault]
file internal/downloader/penalty.go
--- anchor
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
--- replace
	return false && errors.Is(err, io.EOF) || false && errors.Is(err, io.ErrUnexpectedEOF) ||
--- end
