package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/job"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/unpack"
	"github.com/hobeone/gonzbd/internal/unwanted"
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
//	quickcheck → repair → rarvolrecovery → unpack → extractedrepair →
//	sample → par2names → par2cleanup → deobfuscate → unwantedcleanup →
//	extcleanup → finalize → script
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
		{"ExtractedRepairStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.ExtractedRepairStage); return ok }},
		{"SampleCleanupStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.SampleCleanupStage); return ok }},
		{"RecoverPar2NamesStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.RecoverPar2NamesStage); return ok }},
		{"Par2CleanupStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.Par2CleanupStage); return ok }},
		{"DeobfuscateStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.DeobfuscateStage); return ok }},
		{"UnwantedCleanupStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.UnwantedCleanupStage); return ok }},
		{"ExtensionCleanupStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.ExtensionCleanupStage); return ok }},
		{"FinalizeStage", func(s postproc.Stage) bool { _, ok := s.(*postproc.FinalizeStage); return ok }},
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
	if built.Stages[5] != built.SampleCleanup {
		t.Error("SampleCleanup pointer != stages[5]")
	}
	if built.Stages[7] != built.Par2Cleanup {
		t.Error("Par2Cleanup pointer != stages[7]")
	}
	if built.Stages[8] != built.Deobfuscate {
		t.Error("Deobfuscate pointer != stages[8]")
	}
	if built.Stages[10] != built.ExtensionCleanup {
		t.Error("ExtensionCleanup pointer != stages[10]")
	}
	if built.Stages[11] != built.Finalize {
		t.Error("Finalize pointer != stages[11]")
	}
	if built.Stages[12] != built.Script {
		t.Error("Script pointer != stages[12]")
	}
}

// TestBuildStages_UnwantedCleanupReadsLiveSettings pins that the wired
// unwanted_cleanup stage reads the downloads settings at run time rather
// than a copy taken at construction: turning the action off spares the
// file, turning it back on removes it.
func TestBuildStages_UnwantedCleanupReadsLiveSettings(t *testing.T) {
	t.Parallel()
	cfg := convertConfig(Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()})
	setAction := func(action unwanted.Action) {
		cfg.With(func(c *config.Config) {
			c.Downloads.ActionOnUnwantedExtensions = action
			c.Downloads.UnwantedExtensionsMode = unwanted.ModeBlacklist
			c.Downloads.UnwantedExtensions = []string{"exe"}
		})
	}
	// Valid and on at construction, so a stage that kept a construction-time
	// copy would still remove the file once the action is turned off.
	setAction(unwanted.ActionPause)
	built, err := buildStages(cfg, "", discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	stage := built.Stages[9]
	if stage.Name() != "unwanted_cleanup" {
		t.Fatalf("stages[9] = %s, want unwanted_cleanup", stage.Name())
	}
	run := func(action unwanted.Action) bool {
		t.Helper()
		setAction(action)
		dir := t.TempDir()
		exe := filepath.Join(dir, "setup.exe")
		if err := os.WriteFile(exe, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		job := &postproc.Job{Job: job.New("unwantedlive0001", "live", job.PolicyFromPP(3)), DownloadDir: dir}
		if err := stage.Run(t.Context(), job); err != nil {
			t.Fatalf("Run: %v", err)
		}
		_, statErr := os.Stat(exe)
		return statErr == nil
	}
	if !run(unwanted.ActionOff) {
		t.Error("setup.exe removed with the action off")
	}
	if run(unwanted.ActionPause) {
		t.Error("setup.exe survived with the action on")
	}
}

// extracted_repair must repair with the pipeline's own repair stage, so a
// runtime change to the repair settings reaches both passes.
func TestBuildStages_ExtractedRepairUsesThePipelinesRepairStage(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	extracted, ok := built.Stages[4].(*postproc.ExtractedRepairStage)
	if !ok {
		t.Fatalf("stages[4] = %T, want *postproc.ExtractedRepairStage", built.Stages[4])
	}
	if extracted.Repair != built.Repair {
		t.Errorf("ExtractedRepairStage.Repair = %p, want the pipeline's repair stage %p", extracted.Repair, built.Repair)
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

// Quickcheck lets repair defer to unpack only when it can see that unpack
// will run; left unwired it never can, and every Layout B post fails again.
func TestBuildStages_QuickCheckSeesThePipelinesUnpackStage(t *testing.T) {
	t.Parallel()

	cfg := Config{DownloadDir: t.TempDir(), CompleteDir: t.TempDir()}
	built, err := testBuildStages(cfg, discardLog(), emptyProbe())
	if err != nil {
		t.Fatalf("buildStages: %v", err)
	}
	if built.QuickCheck.Unpack != built.Unpack {
		t.Errorf("QuickCheck.Unpack = %p, want the pipeline's unpack stage %p", built.QuickCheck.Unpack, built.Unpack)
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

// TestBuildStages_ProbeHasNoEffectOnEnablement verifies that probe.UnrarInfo
// does not decide whether the unpack stage runs. An available unrar carrying
// HasProblem leaves the stage enabled: HasProblem selects degraded behaviour
// inside the stage, while enablement is config-driven (EnableUnrar).
//
// The two are separable, so a probe that reports a problem must not silently
// turn unpacking off — that would present a configuration failure as a
// missing feature.
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
