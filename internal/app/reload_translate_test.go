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

// TestUnpackConfigFromPP_ForwardsProbeUnrarVersion pins that the detected
// unrar version reaches the unpack stage, which passes -ol- (create no
// symlinks) only from the version that has it.
func TestUnpackConfigFromPP_ForwardsProbeUnrarVersion(t *testing.T) {
	t.Parallel()
	probe := binaryProbe{UnrarInfo: unpack.UnrarInfo{Available: true, Version: 712}}
	got := unpackConfigFromPP(config.PostProcConfig{}, probe, cmdutil.CmdConfig{}, nil)
	if got.Base.UnrarVersion != 712 {
		t.Errorf("Base.UnrarVersion = %d, want 712", got.Base.UnrarVersion)
	}
}

// TestUnpackConfigFromPP_ForwardsArchiveTypeToggles pins that each
// postproc.enable_* archive toggle reaches its own UnpackConfig field: with
// one turned off and the rest on, exactly that one arrives off (#947).
func TestUnpackConfigFromPP_ForwardsArchiveTypeToggles(t *testing.T) {
	t.Parallel()
	all := config.PostProcConfig{EnableUnrar: true, Enable7zip: true, EnableFileJoin: true, EnableTar: true}
	cases := []struct {
		name string
		off  func(*config.PostProcConfig)
		want [4]bool // unrar, 7zip, filejoin, tar
	}{
		{"enable_unrar", func(p *config.PostProcConfig) { p.EnableUnrar = false }, [4]bool{false, true, true, true}},
		{"enable_7zip", func(p *config.PostProcConfig) { p.Enable7zip = false }, [4]bool{true, false, true, true}},
		{"enable_filejoin", func(p *config.PostProcConfig) { p.EnableFileJoin = false }, [4]bool{true, true, false, true}},
		{"enable_tar", func(p *config.PostProcConfig) { p.EnableTar = false }, [4]bool{true, true, true, false}},
	}
	for _, tc := range cases {
		pp := all
		tc.off(&pp)
		c := unpackConfigFromPP(pp, binaryProbe{}, cmdutil.CmdConfig{}, nil)
		if got := [4]bool{c.EnableUnrar, c.Enable7zip, c.EnableFileJoin, c.EnableTar}; got != tc.want {
			t.Errorf("%s off: (unrar, 7zip, filejoin, tar) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUnpackStageEnabled_AnyArchiveType pins that the unpack stage runs when
// any one archive type is enabled, and not when none is.
func TestUnpackStageEnabled_AnyArchiveType(t *testing.T) {
	t.Parallel()
	if unpackStageEnabled(config.PostProcConfig{}) {
		t.Error("unpackStageEnabled with every archive type off = true, want false")
	}
	for name, on := range map[string]func(*config.PostProcConfig){
		"enable_unrar":    func(p *config.PostProcConfig) { p.EnableUnrar = true },
		"enable_7zip":     func(p *config.PostProcConfig) { p.Enable7zip = true },
		"enable_filejoin": func(p *config.PostProcConfig) { p.EnableFileJoin = true },
		"enable_tar":      func(p *config.PostProcConfig) { p.EnableTar = true },
	} {
		var pp config.PostProcConfig
		on(&pp)
		if !unpackStageEnabled(pp) {
			t.Errorf("unpackStageEnabled with only %s on = false, want true", name)
		}
	}
}

// TestRarDecodeWorkersPlumbing pins that rar_decode_workers reaches both the
// post-process unpack options and the DirectUnpack options.
func TestRarDecodeWorkersPlumbing(t *testing.T) {
	t.Parallel()
	pp := config.PostProcConfig{RarDecodeWorkers: 3}
	got := unpackConfigFromPP(pp, binaryProbe{}, cmdutil.CmdConfig{}, nil)
	if got.Base.DecodeWorkers != 3 {
		t.Errorf("unpackConfigFromPP Base.DecodeWorkers = %d, want 3", got.Base.DecodeWorkers)
	}
	du := (&directUnpackOrchestrator{}).buildOpts(&config.Config{PostProc: pp})
	if du.DecodeWorkers != 3 {
		t.Errorf("buildOpts DecodeWorkers = %d, want 3", du.DecodeWorkers)
	}
}

// TestDirectUnpackOptsFromConfig pins that each setting DirectUnpack takes
// reaches its options, downloads.min_free_space among them.
func TestDirectUnpackOptsFromConfig(t *testing.T) {
	t.Parallel()
	snap := &config.Config{
		Downloads: config.DownloadConfig{MinFreeSpace: 5 << 20},
		PostProc: config.PostProcConfig{
			FlatUnpack: true, OverwriteFiles: true, IgnoreUnrarDates: true, RarDecodeWorkers: 2,
		},
	}
	got := (&directUnpackOrchestrator{}).buildOpts(snap)
	if got.MinFreeSpace != 5<<20 || !got.OneFolder || !got.OverwriteFiles || !got.IgnoreUnrarDates || got.DecodeWorkers != 2 {
		t.Errorf("buildOpts = %+v, want MinFreeSpace %d, OneFolder, OverwriteFiles, IgnoreUnrarDates, DecodeWorkers 2",
			got, 5<<20)
	}
}
