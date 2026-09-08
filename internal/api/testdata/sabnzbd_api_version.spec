pkg ./internal/api/
run TestModeVersion

[the endpoint reports gonzbd's build version again — the shipped bug]
file internal/api/router.go
--- anchor
	respondOK(w, "version", sabnzbdAPIVersion)
--- replace
	respondOK(w, "version", s.version)
--- end

[a parseable version that still falls below Sonarr's HasVersion(2, 0) gate]
file internal/api/router.go
--- anchor
const sabnzbdAPIVersion = "4.5.3"
--- replace
const sabnzbdAPIVersion = "1.1.0"
--- end

[a constant Sonarr's regex cannot parse at all]
file internal/api/router.go
--- anchor
const sabnzbdAPIVersion = "4.5.3"
--- replace
const sabnzbdAPIVersion = "dev"
--- end
