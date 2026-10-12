pkg ./internal/job/
run TestClampPP|TestPP_ZeroIsDownloadOnly

[ClampPP has no lower bound]
file internal/job/pp.go
--- anchor
	return PP{level: uint8(min(max(n, 0), maxPP))} //nolint:gosec // G115: min/max bound it to [0,3]
--- replace
	return PP{level: uint8(min(n, maxPP))} //nolint:gosec // G115: min/max bound it to [0,3]
--- end

[ClampPP has no upper bound]
file internal/job/pp.go
--- anchor
	return PP{level: uint8(min(max(n, 0), maxPP))} //nolint:gosec // G115: min/max bound it to [0,3]
--- replace
	return PP{level: uint8(max(n, 0))} //nolint:gosec // G115: min/max bound it to [0,3]
--- end

[ClampPP saturates one level early]
file internal/job/pp.go
--- anchor
	return PP{level: uint8(min(max(n, 0), maxPP))} //nolint:gosec // G115: min/max bound it to [0,3]
--- replace
	return PP{level: uint8(min(max(n, 0), maxPP-1))} //nolint:gosec // G115: min/max bound it to [0,3]
--- end

[Policy ignores the level]
file internal/job/pp.go
--- anchor
func (p PP) Policy() Policy { return PolicyFromPP(p.Int()) }
--- replace
func (p PP) Policy() Policy { return PolicyFromPP(3) }
--- end
