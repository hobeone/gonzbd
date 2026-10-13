pkg ./internal/job/
run TestClampPP|TestPP_ZeroIsDownloadOnly

[ClampPP has no lower bound]
file internal/job/pp.go
--- anchor
	case n < 0:
		return PP{}
--- replace
	case false:
		return PP{}
--- end

[ClampPP has no upper bound]
file internal/job/pp.go
--- anchor
	case n > maxPP:
		return PP{level: maxPP}
--- replace
	case false:
		return PP{level: maxPP}
--- end

[ClampPP saturates one level early]
file internal/job/pp.go
--- anchor
	case n > maxPP:
		return PP{level: maxPP}
--- replace
	case n > maxPP-1:
		return PP{level: maxPP - 1}
--- end

[Policy ignores the level]
file internal/job/pp.go
--- anchor
func (p PP) Policy() Policy { return PolicyFromPP(p.Int()) }
--- replace
func (p PP) Policy() Policy { return PolicyFromPP(3) }
--- end
