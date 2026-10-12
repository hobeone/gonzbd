pkg ./internal/par2/
run ^TestDetectCapabilities$

# DetectCapabilities reads -N, -B, turbo and the version from par2's -h and
# -V output, and a binary that cannot run yields zero caps.

[-N support is not detected]
file internal/par2/detect.go
--- anchor
		caps.SupportsNoDataSkipping = true
--- replace
		caps.SupportsNoDataSkipping = false
--- end

[the version keeps the "version" prefix]
file internal/par2/detect.go
--- anchor
		caps.Version = strings.TrimSpace(versionStr[idx+len("version"):])
--- replace
		caps.Version = strings.TrimSpace(versionStr[idx:])
--- end

[a bare version line is dropped]
file internal/par2/detect.go
--- anchor
	} else if versionStr != "" && len(versionStr) < 200 {
--- replace
	} else if false {
--- end
