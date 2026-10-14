pkg ./internal/unpack/
run ^TestMkdirAllTracked$

[mkdirAllTracked records no missing level]
file internal/unpack/snapshot.go
--- anchor
	*created = append(*created, missing...)
--- replace
	_ = missing
--- end

[mkdirAllTracked records the missing levels only when MkdirAll succeeds]
file internal/unpack/snapshot.go
--- anchor
	*created = append(*created, missing...)
--- replace
	if err == nil {
		*created = append(*created, missing...)
	}
--- end

[mkdirAllTracked does not return early for a known directory]
file internal/unpack/snapshot.go
--- anchor
	if _, ok := known[rel]; ok {
		return nil
	}
--- replace
--- end
