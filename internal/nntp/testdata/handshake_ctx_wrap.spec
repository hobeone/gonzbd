pkg ./internal/nntp/
run TestDialHandshakeCtxCancelSurfacesAsContextCanceled|TestDialHandshakeCtxDeadlineSurfacesAsDeadlineExceeded|TestDialHandshakeServerRejectionNotWrappedWhenCtxEndsBeforeTheCheck

[the ctx-ended check guarding Dial's handshake-failure wrap dropped]
file internal/nntp/conn.go
--- anchor
		if ctxErr := handshakeCtx.Err(); ctxErr != nil && errors.Is(err, os.ErrDeadlineExceeded) {
--- replace
		if ctxErr := handshakeCtx.Err(); false && errors.Is(err, os.ErrDeadlineExceeded) {
--- end

[the errors.Is(err, os.ErrDeadlineExceeded) discriminator dropped, leaving only ctx-ended]
file internal/nntp/conn.go
--- anchor
		if ctxErr := handshakeCtx.Err(); ctxErr != nil && errors.Is(err, os.ErrDeadlineExceeded) {
--- replace
		if ctxErr := handshakeCtx.Err(); ctxErr != nil && (errors.Is(err, os.ErrDeadlineExceeded) || true) {
--- end
