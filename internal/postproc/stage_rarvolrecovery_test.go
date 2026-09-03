package postproc

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hobeone/gonzbd/internal/unpack"
)

func copyRARFixture(t *testing.T, srcName, dstPath string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "unpack", "testdata", srcName))
	if err != nil {
		t.Fatalf("read fixture %s: %v", srcName, err)
	}
	if err := os.WriteFile(dstPath, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dstPath, err)
	}
}

// TestRarVolumeRecoveryStage_RecoversFullyObfuscatedSet proves the stage
// renames a RAR5 volume set with zero filename clues (no PAR2 set present)
// into canonical part-numbered names, so the immediately-following
// UnpackStage's own unpack.Scan() can find and extract it.
func TestRarVolumeRecoveryStage_RecoversFullyObfuscatedSet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obfuscated := map[string]string{
		"multi_new.part01.rar": "aaaaaaaaaaaa.dat",
		"multi_new.part02.rar": "bbbbbbbbbbbb.dat",
		"multi_new.part03.rar": "cccccccccccc.dat",
	}
	for canonical, name := range obfuscated {
		copyRARFixture(t, canonical, filepath.Join(dir, name))
	}

	before, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("unpack.Scan (before): %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("unpack.Scan found %d archive(s) before recovery; want 0", len(before))
	}

	stage := NewRarVolumeRecoveryStage()
	stage.SetEnabled(true)
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Job: newQueueJob(t, "test", 0), OwnedFiles: map[string]struct{}{}}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RarVolumeRecoveryStage.Run: %v", err)
	}

	after, err := unpack.Scan(dir)
	if err != nil {
		t.Fatalf("unpack.Scan (after): %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("unpack.Scan found %d archive(s) after recovery; want exactly 1", len(after))
	}
	if got := len(after[0].Parts); got != 3 {
		t.Errorf("recovered archive has %d part(s); want 3", got)
	}
}

// TestRarVolumeRecoveryStage_NoOpWhenScanFindsArchives proves the stage does
// nothing when normal filename-based detection already works -- the
// overwhelmingly common case, and the fast path this stage must not slow down.
func TestRarVolumeRecoveryStage_NoOpWhenScanFindsArchives(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyRARFixture(t, "multi_new.part01.rar", filepath.Join(dir, "multi_new.part01.rar"))
	copyRARFixture(t, "multi_new.part02.rar", filepath.Join(dir, "multi_new.part02.rar"))

	stage := NewRarVolumeRecoveryStage()
	stage.SetEnabled(true)
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Job: newQueueJob(t, "test", 0), OwnedFiles: map[string]struct{}{}}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RarVolumeRecoveryStage.Run: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "multi_new.part01.rar")); statErr != nil {
		t.Errorf("original filename was renamed away even though Scan already found archives: %v", statErr)
	}
}

// TestRarVolumeRecoveryStage_AmbiguousVolumeCollisionSkipsAll proves that when
// two different obfuscated candidates both resolve to the same recovered
// volume index (here, volume 0 -- one via a genuine single-volume RAR5
// archive, which always normalizes to volume index 0, and the other via the
// first volume of a multi-volume set, which reports volume index 0 through
// the "first volume, flag omitted" header path), the stage logs a warning and
// renames neither file, per the documented ambiguous-collision behavior.
func TestRarVolumeRecoveryStage_AmbiguousVolumeCollisionSkipsAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	firstObfuscated := filepath.Join(dir, "aaaaaaaaaaaa.dat")
	secondObfuscated := filepath.Join(dir, "bbbbbbbbbbbb.dat")
	copyRARFixture(t, "single_rar5.rar", firstObfuscated)
	copyRARFixture(t, "multi_new.part01.rar", secondObfuscated)

	stage := NewRarVolumeRecoveryStage()
	stage.SetEnabled(true)
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Job: newQueueJob(t, "test", 0), OwnedFiles: map[string]struct{}{}}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RarVolumeRecoveryStage.Run: %v", err)
	}

	if _, statErr := os.Stat(firstObfuscated); statErr != nil {
		t.Errorf("first ambiguous candidate was renamed away: %v", statErr)
	}
	if _, statErr := os.Stat(secondObfuscated); statErr != nil {
		t.Errorf("second ambiguous candidate was renamed away: %v", statErr)
	}
}

// TestRarVolumeRecoveryStage_DisabledIsNoOp proves SetEnabled(false) skips
// recovery entirely.
func TestRarVolumeRecoveryStage_DisabledIsNoOp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	copyRARFixture(t, "multi_new.part01.rar", filepath.Join(dir, "aaaaaaaaaaaa.dat"))
	copyRARFixture(t, "multi_new.part02.rar", filepath.Join(dir, "bbbbbbbbbbbb.dat"))

	stage := NewRarVolumeRecoveryStage()
	stage.SetEnabled(false)
	stage.Log = slog.New(slog.DiscardHandler)

	job := &Job{DownloadDir: dir, Job: newQueueJob(t, "test", 0), OwnedFiles: map[string]struct{}{}}
	if err := stage.Run(context.Background(), job); err != nil {
		t.Fatalf("RarVolumeRecoveryStage.Run: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, "aaaaaaaaaaaa.dat")); statErr != nil {
		t.Errorf("file was renamed even though stage is disabled: %v", statErr)
	}
}
