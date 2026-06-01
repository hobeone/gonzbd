package app

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// discardLog returns a logger that throws away all output, keeping test output clean.
func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// emptyProbe returns a binaryProbe with zero values (no external binaries available).
func emptyProbe() binaryProbe {
	return binaryProbe{}
}

// ---------- Stage ordering ----------

// TestBuildStages_StageOrder verifies the documented pipeline order:
//
//	quickcheck → repair → unpack → sample → par2names → par2cleanup
//	→ deobfuscate → extcleanup → finalize → cleanup → script
//
// This is the highest-value assertion for stages.go: a silent reorder would
// cause post-processing failures (e.g. cleanup running before rename).
func TestBuildStages_StageOrder(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}

	type stageCheck struct {
		name string
		fn   func(postproc.Stage) bool
	}
	checks := []stageCheck{
		{"QuickCheckStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.QuickCheckStage); return ok }},
		{"RepairStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.RepairStage); return ok }},
		{"UnpackStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.UnpackStage); return ok }},
		{"SampleCleanupStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.SampleCleanupStage); return ok }},
		{"RecoverPar2NamesStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.RecoverPar2NamesStage); return ok }},
		{"Par2CleanupStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.Par2CleanupStage); return ok }},
		{"DeobfuscateStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.DeobfuscateStage); return ok }},
		{"ExtensionCleanupStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.ExtensionCleanupStage); return ok }},
		{"FinalizeStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.FinalizeStage); return ok }},
		{"CleanupStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.CleanupStage); return ok }},
		{"ScriptStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.ScriptStage); return ok }},
	}

	if len(built.Stages) != len(checks) {
		t.Fatalf("stage count: got %d, want %d", len(built.Stages), len(checks))
	}
	for i, chk := range checks {
		if !chk.fn(built.Stages[i]) {
			t.Errorf("stages[%d]: got %T, want %s", i, built.Stages[i], chk.name)
		}
	}
}

// TestBuildStages_PointersSameAsSlice verifies the individually-addressable
// stage pointers in builtStages point to the same objects as built.Stages.
// If New() stores the pointer but not the slice entry (or vice versa), runtime
// toggling via SetEnabled/SetCleanup will affect the wrong object.
func TestBuildStages_PointersSameAsSlice(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}

	if built.Stages[0] != built.QuickCheck {
		t.Error("QuickCheck pointer != stages[0]")
	}
	if built.Stages[1] != built.Repair {
		t.Error("Repair pointer != stages[1]")
	}
	if built.Stages[2] != built.Unpack {
		t.Error("Unpack pointer != stages[2]")
	}
	if built.Stages[3] != built.SampleCleanup {
		t.Error("SampleCleanup pointer != stages[3]")
	}
	if built.Stages[5] != built.Par2Cleanup {
		t.Error("Par2Cleanup pointer != stages[5]")
	}
	if built.Stages[6] != built.Deobfuscate {
		t.Error("Deobfuscate pointer != stages[6]")
	}
	if built.Stages[8] != built.Finalize {
		t.Error("Finalize pointer != stages[8]")
	}
	if built.Stages[10] != built.Script {
		t.Error("Script pointer != stages[10]")
	}
}

// ---------- Error paths ----------

func TestBuildStages_BadExtraPar2Params(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:    t.TempDir(),
		CompleteDir:    t.TempDir(),
		ExtraPar2Params: "noDash", // must start with '-'
	}
	_, err := buildStages(cfg, discardLog(), emptyProbe())
	if err == nil {
		t.Fatal("expected error for bad ExtraPar2Params, got nil")
	}
}

func TestBuildStages_BadExtraUnrarParams(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:     t.TempDir(),
		CompleteDir:     t.TempDir(),
		ExtraUnrarParams: "noDash", // must start with '-'
	}
	_, err := buildStages(cfg, discardLog(), emptyProbe())
	if err == nil {
		t.Fatal("expected error for bad ExtraUnrarParams, got nil")
	}
}

// ---------- Enablement via config flags ----------

func TestBuildStages_QuickCheckDefaultEnabled(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if !built.QuickCheck.IsEnabled() {
		t.Error("QuickCheckStage: expected enabled when SkipQuickCheck=false")
	}
}

func TestBuildStages_SkipQuickCheckDisablesStage(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:    t.TempDir(),
		CompleteDir:    t.TempDir(),
		SkipQuickCheck: true,
	}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if built.QuickCheck.IsEnabled() {
		t.Error("QuickCheckStage: expected disabled when SkipQuickCheck=true")
	}
}

func TestBuildStages_UnpackEnabledByEnableUnrar(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir: t.TempDir(),
		CompleteDir: t.TempDir(),
		EnableUnrar: true,
	}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if !built.Unpack.IsEnabled() {
		t.Error("UnpackStage: expected enabled when EnableUnrar=true")
	}
}

func TestBuildStages_UnpackEnabledByEnable7zip(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir: t.TempDir(),
		CompleteDir: t.TempDir(),
		Enable7zip:  true,
	}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if !built.Unpack.IsEnabled() {
		t.Error("UnpackStage: expected enabled when Enable7zip=true")
	}
}

func TestBuildStages_UnpackEnabledByEnableFileJoin(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:   t.TempDir(),
		CompleteDir:   t.TempDir(),
		EnableFileJoin: true,
	}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if !built.Unpack.IsEnabled() {
		t.Error("UnpackStage: expected enabled when EnableFileJoin=true")
	}
}

func TestBuildStages_UnpackDisabledByDefault(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if built.Unpack.IsEnabled() {
		t.Error("UnpackStage: expected disabled when no unpack option set")
	}
}

func TestBuildStages_SampleCleanupEnabledByIgnoreSamples(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:  t.TempDir(),
		CompleteDir:  t.TempDir(),
		IgnoreSamples: true,
	}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if !built.SampleCleanup.IsEnabled() {
		t.Error("SampleCleanupStage: expected enabled when IgnoreSamples=true")
	}
}

func TestBuildStages_DeobfuscateEnabledByConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:          t.TempDir(),
		CompleteDir:          t.TempDir(),
		DeobfuscateFilenames: true,
	}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if !built.Deobfuscate.IsEnabled() {
		t.Error("DeobfuscateStage: expected enabled when DeobfuscateFilenames=true")
	}
}

func TestBuildStages_Par2CleanupEnabledByConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:     t.TempDir(),
		CompleteDir:     t.TempDir(),
		EnableParCleanup: true,
	}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if !built.Par2Cleanup.CleanupEnabled() {
		t.Error("Par2CleanupStage: expected cleanup enabled when EnableParCleanup=true")
	}
}

func TestBuildStages_Par2CleanupDisabledByDefault(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := buildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if built.Par2Cleanup.CleanupEnabled() {
		t.Error("Par2CleanupStage: expected cleanup disabled by default")
	}
}

// ---------- Probe propagation ----------

// TestBuildStages_UnrarHasProblemPropagated verifies that probe.UnrarInfo.HasProblem
// flows into the unpack stage. The stage exposes HasProblem via UnpackOpts() in
// export_test.go; this test confirms the value survives construction.
func TestBuildStages_ProbeHasNoEffectOnEnablement(t *testing.T) {
	t.Parallel()

	// An available unrar with HasProblem should not disable the stage —
	// enablement is config-driven (EnableUnrar), not probe-driven.
	probe := binaryProbe{
		UnrarInfo: unpack.UnrarInfo{Available: true, HasProblem: true},
		Par2Caps:  par2.Par2Caps{},
	}
	cfg := Config{
		DownloadDir: t.TempDir(),
		CompleteDir: t.TempDir(),
		EnableUnrar: true,
	}
	built, err := buildStages(cfg, discardLog(), probe)
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	// Unpack should still be enabled — probe.HasProblem affects degraded mode, not toggle.
	if !built.Unpack.IsEnabled() {
		t.Error("UnpackStage: probe.HasProblem should not disable the stage")
	}
}
