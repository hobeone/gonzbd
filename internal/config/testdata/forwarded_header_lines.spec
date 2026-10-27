pkg ./internal/config/
run ^(TestIsTrustedRequest|TestForwardedProtos|TestForwardedParams|TestParseForwardedFor)$

[X-Forwarded-For read from its first line only]
file internal/config/trust.go
--- anchor
		XForwardedFor: joinedValues(h, "X-Forwarded-For"),
--- replace
		XForwardedFor: h.Get("X-Forwarded-For"),
--- end

[Forwarded read from its first line only]
file internal/config/trust.go
--- anchor
		Forwarded:     joinedValues(h, "Forwarded"),
--- replace
		Forwarded:     h.Get("Forwarded"),
--- end

[X-Real-IP read from its first line only]
file internal/config/trust.go
--- anchor
		XRealIP:       joinedValues(h, "X-Real-IP"),
--- replace
		XRealIP:       h.Get("X-Real-IP"),
--- end

[ForwardedProtos reads the first Forwarded line only]
file internal/config/trust.go
--- anchor
	return forwardedParams(joinedValues(h, "Forwarded"), "proto")
--- replace
	return forwardedParams(h.Get("Forwarded"), "proto")
--- end

[Forwarded elements no longer split on commas]
file internal/config/trust.go
--- anchor
	for elem := range strings.SplitSeq(v, ",") {
--- replace
	for elem := range strings.SplitSeq(v, "|") {
--- end

[every matching pair of an element taken, not the first]
file internal/config/trust.go
--- anchor
			vals = append(vals, val)
			break
--- replace
			vals = append(vals, val)
--- end
