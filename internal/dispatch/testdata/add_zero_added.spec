pkg ./internal/dispatch/
run TestAdd_NormalizesZeroOrNegativeAdded

[Added not normalized on <= 0]
file internal/dispatch/registry.go
--- anchor
	if h.Added <= 0 {
		h.Added = time.Now().UTC().Unix()
	}
--- replace
	if false {
		h.Added = time.Now().UTC().Unix()
	}
--- end
