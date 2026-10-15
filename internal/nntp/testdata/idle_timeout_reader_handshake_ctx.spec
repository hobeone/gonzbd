pkg ./internal/nntp/
run TestIdleTimeoutReaderForcesDeadlineWhenHandshakeCtxAlreadyDone

[the handshakeCtx-done check in idleTimeoutReader.Read dropped, restoring the pre-fix unconditional re-arm]
file internal/nntp/conn.go
--- anchor
	if ctx := r.handshakeCtx; ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
--- replace
	if ctx := r.handshakeCtx; ctx != nil {
		if false {
--- end
