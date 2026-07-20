package app

import (
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

// These tests characterize (pin) the existing behaviour of the Reload*Options
// family from before Step 1 of #109 collapsed the 9 concrete stage-pointer
// fields on Application (formerly app.unpackStage, app.repairStage, ...) into
// one held builtStages struct (app.stages). They assert
// through the stable white-box accessors added in export_test.go rather than
// through the fields directly, so they survived that relocation unchanged.
//
// No bug is being fixed here: these tests are expected to pass on current,
// unmodified code by construction.

// TestApplication_ReloadPostProcOptions_Characterization drives
// ReloadPostProcOptions end-to-end and asserts, through the stable
// accessors, that each hot-reloadable field actually changes when the
// snapshot changes -- and that a field left unchanged stays put. Asserting
// the pre-reload value too proves the reload is what caused the change, not
// that the accessor always reports that value.
func TestApplication_ReloadPostProcOptions_Characterization(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	cfg.With(func(c *config.Config) {
		c.PostProc.StrictSandbox = false
		c.PostProc.UseGoRAR = false
		c.PostProc.EnableFileJoin = false
		c.PostProc.Par2Turbo = false
	})
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	// Precondition: all four start false.
	if got := app.StageStrictSandbox(); got {
		t.Fatalf("precondition: StageStrictSandbox() = %v, want false", got)
	}
	if got := app.StageUseGoRAR(); got {
		t.Fatalf("precondition: StageUseGoRAR() = %v, want false", got)
	}
	if got := app.StageEnableFileJoin(); got {
		t.Fatalf("precondition: StageEnableFileJoin() = %v, want false", got)
	}
	if got := app.StagePar2Turbo(); got {
		t.Fatalf("precondition: StagePar2Turbo() = %v, want false", got)
	}

	// Flip StrictSandbox, UseGoRAR, and Par2Turbo to true; leave
	// EnableFileJoin false (unchanged) to prove the reload doesn't just
	// blanket-flip everything to true.
	app.ReloadPostProcOptions(config.PostProcConfig{
		StrictSandbox:  true,
		UseGoRAR:       true,
		EnableFileJoin: false,
		Par2Turbo:      true,
	}, "")

	if got := app.StageStrictSandbox(); !got {
		t.Errorf("after reload: StageStrictSandbox() = %v, want true", got)
	}
	if got := app.StageUseGoRAR(); !got {
		t.Errorf("after reload: StageUseGoRAR() = %v, want true", got)
	}
	if got := app.StageEnableFileJoin(); got {
		t.Errorf("after reload: StageEnableFileJoin() = %v, want false (unchanged)", got)
	}
	if got := app.StagePar2Turbo(); !got {
		t.Errorf("after reload: StagePar2Turbo() = %v, want true", got)
	}

	// Reload again with EnableFileJoin now flipped on, and the others
	// flipped back off, to prove the accessors track subsequent reloads
	// rather than latching the first observed value.
	app.ReloadPostProcOptions(config.PostProcConfig{
		StrictSandbox:  false,
		UseGoRAR:       false,
		EnableFileJoin: true,
		Par2Turbo:      false,
	}, "")

	if got := app.StageStrictSandbox(); got {
		t.Errorf("after second reload: StageStrictSandbox() = %v, want false", got)
	}
	if got := app.StageUseGoRAR(); got {
		t.Errorf("after second reload: StageUseGoRAR() = %v, want false", got)
	}
	if got := app.StageEnableFileJoin(); !got {
		t.Errorf("after second reload: StageEnableFileJoin() = %v, want true", got)
	}
	if got := app.StagePar2Turbo(); got {
		t.Errorf("after second reload: StagePar2Turbo() = %v, want false", got)
	}
}

// TestApplication_ReloadDownloadOptions_Characterization drives
// ReloadDownloadOptions end-to-end and asserts, through
// AssemblerMinFreeBytes, that the running assembler's low-disk-space
// threshold reflects the reloaded config.DownloadConfig.MinFreeSpace value.
func TestApplication_ReloadDownloadOptions_Characterization(t *testing.T) {
	cfg := testConfig(t.TempDir(), t.TempDir(), t.TempDir())
	cfg.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = 0
	})
	app, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Shutdown()

	if got := app.AssemblerMinFreeBytes(); got != 0 {
		t.Fatalf("precondition: AssemblerMinFreeBytes() = %d, want 0", got)
	}

	cfg.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = config.ByteSize(100 * 1024 * 1024)
	})
	app.ReloadDownloadOptions(cfg.Downloads)

	if got := app.AssemblerMinFreeBytes(); got != 100*1024*1024 {
		t.Errorf("after reload: AssemblerMinFreeBytes() = %d, want %d", got, 100*1024*1024)
	}

	// Reload again with a different value to prove the accessor tracks
	// subsequent reloads, not just the first observed change.
	cfg.With(func(c *config.Config) {
		c.Downloads.MinFreeSpace = config.ByteSize(25 * 1024 * 1024)
	})
	app.ReloadDownloadOptions(cfg.Downloads)

	if got := app.AssemblerMinFreeBytes(); got != 25*1024*1024 {
		t.Errorf("after second reload: AssemblerMinFreeBytes() = %d, want %d", got, 25*1024*1024)
	}
}

// ReloadGeneralOptions is intentionally NOT characterized here through a new
// accessor: its only observable effects (globalLevelVar, componentLevels)
// are package-level vars already directly readable in-package (see the
// existing TestApplication_ReloadOptions in reloader_test.go, which asserts
// on them directly). Adding an Application-level accessor for state that
// isn't even an Application field would be a production-shaped abstraction
// with no reload-relocation risk to pin against, so it's out of scope for
// this step.
