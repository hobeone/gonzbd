pkg ./internal/postproc/
run TestUnpackStage_RAR4_GoFirstThenUnrar

# The unpack stage extracts a stored RAR 1.5-4.x set with go_unrar alone, and
# retries a compressed one with the external unrar only under go_rar_fallback.

[go_unrar refuses RAR 1.5-4.x by signature again]
file internal/unpack/go_unrar.go
--- anchor
	// A RAR 1.5-4.x archive (ver 3) goes to rarengine too: it extracts stored
--- replace
	if ver == 3 {
		return Result{Reason: FailUnknown}, rarengine.ErrUnsupportedFormat
	}
	// A RAR 1.5-4.x archive (ver 3) goes to rarengine too: it extracts stored
--- end

[the retry ignores go_rar_fallback]
file internal/postproc/stage_unpack.go
--- anchor
	if goErr != nil && ctx.Err() == nil && d.fallback {
--- replace
	if goErr != nil && ctx.Err() == nil {
--- end
