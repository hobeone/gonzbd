pkg ./internal/api/
run TestModeConfig_Dispatch|TestConfigSpeedLimit_Conversions|TestConfigTestServer_RequestParameters|TestModeGetConfig_SectionSelection

[plain speed limit numbers no longer read as KiB/s]
file internal/api/config.go
--- anchor
		bytesPerSec = n * 1024
--- replace
		bytesPerSec = n
--- end

[ssl no longer moves the default port]
file internal/api/config.go
--- anchor
	if ssl && port == 119 {
--- replace
	if false {
--- end

[an unknown section stops being an empty object]
file internal/api/config.go
--- anchor
		if !ok {
			// Section not found
--- replace
		if !ok && false {
			// Section not found
--- end
