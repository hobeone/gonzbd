pkg ./internal/unpack/
run TestGoUnRAR_RAR4StoredMultiVolume

# GoUnRAR hands a RAR 1.5-4.x archive to rarengine, which extracts its stored
# members, rather than refusing it by its signature.

[RAR 1.5-4.x refused by signature again]
file internal/unpack/go_unrar.go
--- anchor
	// A RAR 1.5-4.x archive (ver 3) goes to rarengine too: it extracts stored
--- replace
	if ver == 3 {
		return Result{Reason: FailUnknown}, rarengine.ErrUnsupportedFormat
	}
	// A RAR 1.5-4.x archive (ver 3) goes to rarengine too: it extracts stored
--- end
