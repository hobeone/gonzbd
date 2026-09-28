pkg ./internal/nntp/
run TestDialHandshakeCtxCancelSurfacesAsContextCanceled|TestDialHandshakeCtxDeadlineSurfacesAsDeadlineExceeded

[the ctx-ended check guarding Dial's handshake-failure wrap dropped]
file internal/nntp/conn.go
--- anchor
		if ctxErr := handshakeCtx.Err(); ctxErr != nil {
--- replace
		if ctxErr := handshakeCtx.Err(); false {
--- end
