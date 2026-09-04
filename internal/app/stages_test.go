package app

import (
	"log/slog"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// discardLog returns a logger that throws away all output, keeping test output clean.
func discardLog() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// emptyProbe returns a binaryProbe with zero values (no external binaries available).
func emptyProbe() binaryProbe {
	return binaryProbe{}
}

// ---------- Stage ordering ----------

// TestBuildStages_StageOrder verifies the documented pipeline order:
//
//	quickcheck → repair → rarvolrecovery → unpack → sample → par2names →
//	par2cleanup → deobfuscate → extcleanup → finalize → cleanup → script
//
// This is the highest-value assertion for stages.go: a silent reorder would
// cause post-processing failures (e.g. cleanup running before rename).
func TestBuildStages_StageOrder(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
		{"RarVolumeRecoveryStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.RarVolumeRecoveryStage); return ok }},
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
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}

	if built.Stages[0] != built.QuickCheck {
		t.Error("QuickCheck pointer != stages[0]")
	}
	if built.Stages[1] != built.Repair {
		t.Error("Repair pointer != stages[1]")
	}
	if built.Stages[3] != built.Unpack {
		t.Error("Unpack pointer != stages[3]")
	}
	if built.Stages[4] != built.SampleCleanup {
		t.Error("SampleCleanup pointer != stages[4]")
	}
	if built.Stages[6] != built.Par2Cleanup {
		t.Error("Par2Cleanup pointer != stages[6]")
	}
	if built.Stages[7] != built.Deobfuscate {
		t.Error("Deobfuscate pointer != stages[7]")
	}
	if built.Stages[9] != built.Finalize {
		t.Error("Finalize pointer != stages[9]")
	}
	if built.Stages[11] != built.Script {
		t.Error("Script pointer != stages[11]")
	}
}

// ---------- Error paths ----------

func TestBuildStages_BadExtraPar2Params(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:     t.TempDir(),
		CompleteDir:     t.TempDir(),
		ExtraPar2Params: "noDash", // must start with '-'
	}
	_, err := testBuildStages(cfg, discardLog(), emptyProbe())
	if err == nil {
		t.Fatal("expected error for bad ExtraPar2Params, got nil")
	}
}

func TestBuildStages_BadExtraUnrarParams(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:      t.TempDir(),
		CompleteDir:      t.TempDir(),
		ExtraUnrarParams: "noDash", // must start with '-'
	}
	_, err := testBuildStages(cfg, discardLog(), emptyProbe())
	if err == nil {
		t.Fatal("expected error for bad ExtraUnrarParams, got nil")
	}
}

func TestBuildStages_DisallowedExtraUnrarParams(t *testing.T) {
	t.Parallel()

	cfg := Config{
		DownloadDir:      t.TempDir(),
		CompleteDir:      t.TempDir(),
		ExtraUnrarParams: "-df", // not allowed
	}
	_, err := testBuildStages(cfg, discardLog(), emptyProbe())
	if err == nil {
		t.Fatal("expected error for disallowed ExtraUnrarParams, got nil")
	}
}

// ---------- Enablement via config flags ----------

func TestBuildStages_QuickCheckDefaultEnabled(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
		DownloadDir:    t.TempDir(),
		CompleteDir:    t.TempDir(),
		EnableFileJoin: true,
	}
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
		DownloadDir:   t.TempDir(),
		CompleteDir:   t.TempDir(),
		IgnoreSamples: true,
	}
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
		DownloadDir:      t.TempDir(),
		CompleteDir:      t.TempDir(),
		EnableParCleanup: true,
	}
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
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
		Par2Caps:  par2.Caps{},
	}
	cfg := Config{
		DownloadDir: t.TempDir(),
		CompleteDir: t.TempDir(),
		EnableUnrar: true,
	}
	built, err := testBuildStages(cfg, discardLog(), probe)
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	// Unpack should still be enabled — probe.HasProblem affects degraded mode, not toggle.
	if !built.Unpack.IsEnabled() {
		t.Error("UnpackStage: probe.HasProblem should not disable the stage")
	}
}

type Config struct {
	DownloadDir          string
	CompleteDir          string
	AdminDir             string
	WriteCacheBytes      int64
	Servers              []config.ServerConfig
	Categories           []config.CategoryConfig
	Nice                 string
	Ionice               string
	ExtraPar2Params      string
	ExtraUnrarParams     string
	Par2Command          string
	Par2Turbo            bool
	UnrarCommand         string
	SevenzCommand        string
	UseGoPar2            bool
	GoPar2Fallback       bool
	UseGoRAR             bool
	GoRarFallback        bool
	UseGo7z              bool
	Go7zFallback         bool
	EnableUnrar          bool
	Enable7zip           bool
	EnableFileJoin       bool
	EnableRecursive      bool
	EnableRarCleanup     bool
	EnableParCleanup     bool
	IgnoreSamples        bool
	DeobfuscateFilenames bool
	CleanupExtensions    []string
	FolderRename         bool
	ScriptDir            string
	ScriptCanFail        bool
	Version              string
	APIKey               string
	ListenAddr           string
	SkipQuickCheck       bool
	Permissions          string
	PasswordFile         string
	IgnoreUnrarDates     bool
	OverwriteFiles       bool
	FlatUnpack           bool
	StrictSandbox        bool
}

func convertConfig(c Config) *config.Config {
	cfg := &config.Config{}
	cfg.With(func(o *config.Config) {
		o.General.DownloadDir = c.DownloadDir
		o.General.CompleteDir = c.CompleteDir
		o.General.AdminDir = c.AdminDir
		o.General.ScriptDir = c.ScriptDir
		o.Downloads.WriteCacheSize = config.ByteSize(c.WriteCacheBytes)
		o.Downloads.ReplaceIllegalWith = "_" // sensible default
		o.PostProc.DeobfuscateFilenames = c.DeobfuscateFilenames
		o.PostProc.IgnoreSamples = c.IgnoreSamples
		o.PostProc.EnableUnrar = c.EnableUnrar
		o.PostProc.Enable7zip = c.Enable7zip
		o.PostProc.EnableFileJoin = c.EnableFileJoin
		o.PostProc.EnableRecursive = c.EnableRecursive
		o.PostProc.EnableParCleanup = c.EnableParCleanup
		o.PostProc.EnableRarCleanup = c.EnableRarCleanup
		o.PostProc.Par2Command = c.Par2Command
		o.PostProc.Par2Turbo = c.Par2Turbo
		o.PostProc.UnrarCommand = c.UnrarCommand
		o.PostProc.SevenzCommand = c.SevenzCommand
		o.PostProc.IgnoreUnrarDates = c.IgnoreUnrarDates
		o.PostProc.OverwriteFiles = c.OverwriteFiles
		o.PostProc.FlatUnpack = c.FlatUnpack
		o.PostProc.UseGoRAR = c.UseGoRAR
		o.PostProc.UseGo7z = c.UseGo7z
		o.PostProc.UseGoPar2 = c.UseGoPar2
		o.PostProc.GoRarFallback = c.GoRarFallback
		o.PostProc.Go7zFallback = c.Go7zFallback
		o.PostProc.GoPar2Fallback = c.GoPar2Fallback
		o.PostProc.EnableQuickCheck = !c.SkipQuickCheck
		o.PostProc.CleanupExtensions = c.CleanupExtensions
		o.PostProc.FolderRename = c.FolderRename
		o.PostProc.Nice = c.Nice
		o.PostProc.Ionice = c.Ionice
		o.PostProc.Permissions = c.Permissions
		o.PostProc.PasswordFile = c.PasswordFile
		o.PostProc.ExtraUnrarParams = c.ExtraUnrarParams
		o.PostProc.ExtraPar2Params = c.ExtraPar2Params
		o.PostProc.ScriptCanFail = c.ScriptCanFail
		o.PostProc.StrictSandbox = c.StrictSandbox
		o.Servers = c.Servers
		o.Categories = c.Categories
	})
	return cfg
}

func testBuildStages(c Config, log *slog.Logger, probe binaryProbe) (builtStages, error) {
	cfg := convertConfig(c)
	return buildStages(cfg, c.Version, log, probe)
}

func TestProbeBinaries(t *testing.T) {
	t.Parallel()
	cfg := convertConfig(Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()})
	p1 := probeBinaries(t.Context(), cfg, discardLog())
	p2 := probeBinaries(t.Context(), cfg, discardLog())
	if p1 != p2 {
		t.Errorf("probeBinaries() = %+v, want cached %+v", p2, p1)
	}
}

