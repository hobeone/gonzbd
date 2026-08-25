package job

import "strings"

// Policy is what a job is permitted to do, resolved once at ingestion from
// SABnzbd's PP level plus the job's category.
//
// PP 0-3 is a cumulative integer mask inherited from upstream, and it is the
// same KIND of thing as upstream's status strings: external vocabulary,
// translated at the boundary and never stored internally. The integer does
// not exist past App (D4).
//
// Every state runs at every policy. At Verify: false the Assessor returns
// Complete without doing work and the job crosses the boundary immediately.
// Gating STATES on the level instead would mean skipping Assessing at PP=0 —
// which removes the only state that decides and leaves nothing to authorize
// the crossing, forcing a second decider back into the design.
//
// The zero value is download-only, so a job built without an explicit policy
// does the least destructive thing.
type Policy struct {
	// Verify permits the Assessor to reach a real verdict. When false it
	// returns Complete unconditionally.
	Verify bool
	// Repair permits entering Repairing.
	Repair bool
	// Unpack permits archive extraction inside Extracting.
	Unpack bool
	// Delete permits removing archives and sidecar files after extraction.
	Delete bool
}

// PolicyFromPP resolves an upstream PP level to a Policy. Out-of-range input
// clamps: PP arrives from config, from an API query parameter and from an NZB
// meta tag, none of which we control, and clamping is preferable to
// synthesising a combination nobody designed.
func PolicyFromPP(pp int) Policy {
	pp = min(max(pp, 0), 3)
	return Policy{
		Verify: pp >= 1,
		Repair: pp >= 1,
		Unpack: pp >= 2,
		Delete: pp >= 3,
	}
}

func (p Policy) String() string {
	var on []string
	if p.Verify {
		on = append(on, "verify")
	}
	if p.Repair {
		on = append(on, "repair")
	}
	if p.Unpack {
		on = append(on, "unpack")
	}
	if p.Delete {
		on = append(on, "delete")
	}
	if len(on) == 0 {
		return "Policy(download-only)"
	}
	return "Policy(" + strings.Join(on, ",") + ")"
}
