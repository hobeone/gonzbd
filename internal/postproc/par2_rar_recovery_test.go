package postproc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// TestRepairStage_RecoversObfuscatedRARVolumesViaPar2 proves that a RAR
// volume set with zero filename clues (opaque names, no .rar/.rNN/.partNN.rar
// suffix at all) is invisible to unpack.Scan() before repair, but becomes
// extractable after RepairStage.Run performs its normal PAR2 content-hash
// rename-matching -- using the *default* native Go PAR2 engine (UseGoPar2),
// which relies on github.com/hobeone/par2engine's Decoder.Repair "Phase 0:
// rename misnamed files" step. This is a regression guard for behavior that
// was verified manually (not previously covered by any test): losing it
// would silently reintroduce the "fully obfuscated RAR set never gets
// extracted" bug even though nothing in internal/unpack changed.
func TestRepairStage_RecoversObfuscatedRARVolumesViaPar2(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Copy the 5 canonical RAR volumes the fixture PAR2 set describes,
	// under fully obfuscated names with no recognizable extension.
	obfuscatedNames := map[string]string{
		"multi_new.part01.rar": "a1b2c3d4e5f6.dat",
		"multi_new.part02.rar": "b2c3d4e5f6a1.dat",
		"multi_new.part03.rar": "c3d4e5f6a1b2.dat",
		"multi_new.part04.rar": "d4e5f6a1b2c3.dat",
		"multi_new.part05.rar": "e5f6a1b2c3d4.dat",
	}
	for canonical, obfuscated := range obfuscatedNames {
		data, err := os.ReadFile(filepath.Join("..", "unpack", "testdata", canonical))
		if err != nil {
			t.Fatalf("read fixture %s: %v", canonical, err)
		}
		if err := os.WriteFile(filepath.Join(dir, obfuscated), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", obfuscated, err)
		}
	}

	// Copy the PAR2 recovery set that describes those 5 files by their
	// canonical names and content hashes.
	for _, name := range []string{"recovery.par2", "recovery.vol0+1.par2"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "par2", "rar_obfuscated_recovery", name))
		if err != nil {
			t.Fatalf("read par2 fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	before, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("unpack.Scan (before repair): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("unpack.Scan found %d archive(s) before repair; want 0 (files are fully obfuscated)", len(before))
	}

	stage := NewRepairStage()
	stage.Apply(RepairConfig{UseGoPar2: true}) // matches config default
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Job: newQueueJob(t, "test", 0)}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RepairStage.Run: %v", err)
	}

	after, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("unpack.Scan (after repair): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("unpack.Scan found %d archive(s) after repair; want exactly 1", len(after))
	}
	if got := len(after[0].Parts); got != 5 {
		t.Errorf("recovered archive has %d part(s); want 5", got)
	}
	if got := filepath.Base(after[0].MainFile); got != "multi_new.part01.rar" {
		t.Errorf("recovered archive's MainFile = %q; want multi_new.part01.rar", got)
	}
}
