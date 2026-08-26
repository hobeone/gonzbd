package job

import "testing"

func TestPolicyFromPP(t *testing.T) {
	for _, tc := range []struct {
		name string
		pp   int
		want Policy
	}{
		{"pp0 download only", 0, Policy{}},
		{"pp1 repair", 1, Policy{Verify: true, Repair: true}},
		{"pp2 repair and unpack", 2, Policy{Verify: true, Repair: true, Unpack: true}},
		{"pp3 everything", 3, Policy{Verify: true, Repair: true, Unpack: true, Delete: true}},

		// PP is an external integer and arrives from config, an API query
		// parameter and an NZB meta tag. Out-of-range values clamp rather
		// than producing a policy nobody designed.
		{"negative clamps to pp0", -1, Policy{}},
		{"above range clamps to pp3", 9, Policy{Verify: true, Repair: true, Unpack: true, Delete: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PolicyFromPP(tc.pp); got != tc.want {
				t.Errorf("PolicyFromPP(%d) = %+v, want %+v", tc.pp, got, tc.want)
			}
		})
	}
}

// TestPolicy_ZeroIsDownloadOnly pins that the zero Policy is PP=0 rather than
// "everything on". A job constructed without an explicit policy must do the
// least destructive thing, not the most.
func TestPolicy_ZeroIsDownloadOnly(t *testing.T) {
	var p Policy
	if p != PolicyFromPP(0) {
		t.Errorf("zero Policy = %+v, want PolicyFromPP(0) = %+v", p, PolicyFromPP(0))
	}
	if p.Unpack || p.Delete {
		t.Error("zero Policy enables a destructive step; it must default to download-only")
	}
}

func TestPolicy_String(t *testing.T) {
	for _, tc := range []struct {
		p    Policy
		want string
	}{
		{Policy{}, "Policy(download-only)"},
		{PolicyFromPP(1), "Policy(verify,repair)"},
		{PolicyFromPP(3), "Policy(verify,repair,unpack,delete)"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.p.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
