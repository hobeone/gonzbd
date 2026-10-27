pkg ./internal/api/
run ^(TestRequestScheme_RepeatedForwardingLines|TestCallerLevel_RepeatedForwardedForLine)$

[the cookie gate reads X-Forwarded-For from its first line only]
file internal/config/trust.go
--- anchor
		XForwardedFor: joinedValues(h, "X-Forwarded-For"),
--- replace
		XForwardedFor: h.Get("X-Forwarded-For"),
--- end

[requestScheme reads proto= from the first Forwarded line only]
file internal/api/middleware.go
--- anchor
		for _, proto := range config.ForwardedProtos(r.Header) {
--- replace
		for _, proto := range config.ForwardedProtos(http.Header{"Forwarded": {r.Header.Get("Forwarded")}}) {
--- end
