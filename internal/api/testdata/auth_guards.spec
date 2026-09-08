pkg ./internal/api/
run TestHandleWS|TestGetAuth|TestModeAuth|TestRegisterModes

[the WebSocket handler's own authorization check removed]
file internal/api/server.go
--- anchor
	if callerLevel(r, s.getAuth()) < LevelProtected {
--- replace
	if false {
--- end

[a config-less server hands back a populated AuthConfig instead of failing closed]
file internal/api/server.go
--- anchor
		return AuthConfig{}
--- replace
		return AuthConfig{SessionKey: s.sessionKey}
--- end

[getAuth caches nothing today; make it ignore the live key]
file internal/api/server.go
--- anchor
	auth.APIKey = gen.APIKey
--- replace
	auth.APIKey = ""
--- end

[the nzb key classified as the full api key]
file internal/api/router.go
--- anchor
		respondOK(w, "auth", "nzbkey")
--- replace
		respondOK(w, "auth", "apikey")
--- end

[an unrecognized key accepted as valid]
file internal/api/router.go
--- anchor
		respondOK(w, "auth", "badkey")
--- replace
		respondOK(w, "auth", "apikey")
--- end

[mode=version promoted out of LevelOpen]
file internal/api/router.go
--- anchor
		"version": {handler: s.modeVersion, level: LevelOpen},
--- replace
		"version": {handler: s.modeVersion, level: LevelProtected},
--- end
