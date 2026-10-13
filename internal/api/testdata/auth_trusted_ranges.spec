pkg ./internal/api/
run ^TestGetAuth_ReadsLiveConfig$

[getAuth drops the configured local ranges]
file internal/api/server.go
--- anchor
	auth.TrustedRanges = gen.TrustedRanges()
--- replace
	auth.TrustedRanges = nil
--- end
