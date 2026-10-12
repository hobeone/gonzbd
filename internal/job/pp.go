package job

// maxPP is the highest post-processing level, +delete.
const maxPP = 3

// PP is a post-processing level in SABnzbd's cumulative vocabulary: 0
// download only, 1 +repair, 2 +unpack, 3 +delete (docs/sabnzbd_spec.md
// §8.2). Policy translates it.
//
// The level is unexported so that no PP outside [0,3] can be built outside
// this package: ClampPP saturates any level into that range. The zero value
// is level 0, download only, matching the zero Policy.
type PP struct{ level uint8 }

// ClampPP returns the PP for level n saturated into [0,3]: below 0 is 0, above
// 3 is 3.
func ClampPP(n int) PP {
	if n < 0 {
		n = 0
	} else if n > maxPP {
		n = maxPP
	}
	return PP{level: uint8(n)}
}

// Int returns the level as an integer in [0,3].
func (p PP) Int() int { return int(p.level) }

// Policy returns what the level permits (PolicyFromPP).
func (p PP) Policy() Policy { return PolicyFromPP(p.Int()) }
