pkg ./internal/job/
run TestParsePP|TestClampPP|TestPP_ZeroIsDownloadOnly

[ParsePP admits a level above 3]
file internal/job/pp.go
--- anchor
	if n < 0 || n > maxPP {
--- replace
	if n < 0 || n > maxPP+1 {
--- end

[ParsePP admits a negative level]
file internal/job/pp.go
--- anchor
	if n < 0 || n > maxPP {
--- replace
	if n < -1 || n > maxPP {
--- end

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

[Policy ignores the level]
file internal/job/pp.go
--- anchor
func (p PP) Policy() Policy { return PolicyFromPP(p.Int()) }
--- replace
func (p PP) Policy() Policy { return PolicyFromPP(3) }
--- end
