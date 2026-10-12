pkg ./internal/job/
run TestClampPP|TestPP_ZeroIsDownloadOnly

[ClampPP has no lower bound]
file internal/job/pp.go
--- anchor
	if n < 0 {
		n = 0
	} else if n > maxPP {
--- replace
	if false {
		n = 0
	} else if n > maxPP {
--- end

[ClampPP has no upper bound]
file internal/job/pp.go
--- anchor
	} else if n > maxPP {
		n = maxPP
	}
--- replace
	} else if false {
		n = maxPP
	}
--- end

[ClampPP saturates one level early]
file internal/job/pp.go
--- anchor
	} else if n > maxPP {
		n = maxPP
	}
--- replace
	} else if n > maxPP-1 {
		n = maxPP - 1
	}
--- end

[Policy ignores the level]
file internal/job/pp.go
--- anchor
func (p PP) Policy() Policy { return PolicyFromPP(p.Int()) }
--- replace
func (p PP) Policy() Policy { return PolicyFromPP(3) }
--- end
