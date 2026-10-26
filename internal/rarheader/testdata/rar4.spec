pkg ./internal/rarheader/
run TestInspect_RAR4_|TestInspect_BothRoutesFail_KeepsTheEngineError|TestInspectNativeHelper

# Inspect lists a RAR 1.5-4.x archive through rarengine without forking unrar,
# still falls back to unrar for one rarengine cannot list, and keeps the
# engine's error when the fallback fails too.

[RAR 1.5-4.x routed to unrar again]
file internal/rarheader/rarheader.go
--- anchor
	if nativeErr == nil {
--- replace
	if nativeErr == nil && ver == 5 {
--- end

[an engine failure no longer falls back]
file internal/rarheader/rarheader.go
--- anchor
	info, err = inspectViaUnrar(p, ver)
--- replace
	info, err = Info{Version: ver}, ErrNotRAR
--- end

[directory members listed as files]
file internal/rarheader/rarheader.go
--- anchor
			if !entry.Header.IsDir {
--- replace
			if true {
--- end

[a member's encryption not reported]
file internal/rarheader/rarheader.go
--- anchor
			if entry.Header.Encrypted {
--- replace
			if false {
--- end

[the engine's error is dropped when unrar fails too]
file internal/rarheader/rarheader.go
--- anchor
		return info, errors.Join(nativeErr, err)
--- replace
		return info, err
--- end
