package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// TestUnpackConfigFromPP_ForwardsProbeHasProblem pins the wire between the
// unrar probe and the unpack stage's config.
//
// Both ENDS of this were already tested and the wire between them was not.
// unpack's TestUnRAR_HasProblemDegradedMode covers what a true HasProblem does
// once it is in the config — the modern flags (-scf, -or, -ai, -tsm-) are
// dropped, because a legacy or non-RARLAB unrar rejects them — and version
// detection has its own table for producing the flag. Neither observes
// unpackConfigFromPP, which is what carries the value from one to the other.
//
// The gap was not theoretical: hardcoding this field to false and running
// TestBuildStages left the package green, which is how a refactor that
// dropped the assignment would have read. The symptom in production is
// extraction failing against exactly the binaries the flag exists to
// accommodate, with every test still passing.
//
// unpackConfigFromPP is deliberately the single source of truth for the
// config→unpack mapping — buildStages and ReloadPostProcOptions both call it
// so the two paths cannot drift — so pinning it here covers construction and
// hot reload together.
func TestUnpackConfigFromPP_ForwardsProbeHasProblem(t *testing.T) {
	t.Parallel()

	for _, want := range []bool{true, false} {
		probe := binaryProbe{
			UnrarInfo: unpack.UnrarInfo{Available: true, HasProblem: want},
			Par2Caps:  par2.Caps{},
		}
		got := unpackConfigFromPP(config.PostProcConfig{}, probe, cmdutil.CmdConfig{}, nil)
		if got.Base.HasProblem != want {
			t.Errorf("unpackConfigFromPP with probe HasProblem=%v produced Base.HasProblem=%v; "+
				"the degraded-mode flag never reaches the unpack stage, so a legacy unrar "+
				"is handed flags it rejects", want, got.Base.HasProblem)
		}
	}
}
