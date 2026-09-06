package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/rarheader"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// RarVolumeRecoveryStage renames a fully obfuscated RAR5 volume set (no
// filename numbering clue at all, no PAR2 protection to recover it via
// content hash) into canonical part-numbered names, using the volume
// sequencing embedded in each file's own RAR5 header. Runs immediately
// before UnpackStage; a no-op whenever unpack.Scan already finds at least
// one archive, since that means normal filename-based detection already
// works and this recovery path is unnecessary. Handles exactly one
// obfuscated set per job -- if more than one candidate claims the same
// volume position (ambiguous), it logs a warning and renames nothing.
type RarVolumeRecoveryStage struct {
	toggle
	Log *slog.Logger
}

// NewRarVolumeRecoveryStage constructs a RarVolumeRecoveryStage.
func NewRarVolumeRecoveryStage() *RarVolumeRecoveryStage { return &RarVolumeRecoveryStage{} }

// Name implements Stage.
func (*RarVolumeRecoveryStage) Name() string { return "rar_volume_recovery" }

// Run implements Stage.
func (s *RarVolumeRecoveryStage) Run(ctx context.Context, job *Job) error {
	if !s.enabled() {
		return nil
	}
	log := s.logger(job)

	found, err := unpack.Scan(job.DownloadDir)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "rar_volume_recovery: scan failed: %v", err)
		return nil
	}
	if len(found) > 0 {
		return nil // normal filename-based detection already works
	}

	entries, err := os.ReadDir(job.DownloadDir)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "rar_volume_recovery: read dir failed: %v", err)
		return nil
	}

	byVolume := make(map[int]string) // volume index -> candidate path
	var ambiguous bool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(job.DownloadDir, e.Name())
		isRAR, rarErr := rarheader.IsRAR(p)
		if rarErr != nil || !isRAR {
			continue
		}
		volIdx, multiVol, recErr := rarheader.RecoverVolumeExtension(p)
		if recErr != nil {
			logf(ctx, log, job, slog.LevelInfo, "rar_volume_recovery: skipping %s: %v", e.Name(), recErr)
			continue
		}
		if !multiVol {
			// Deliberate: a standalone (non-split) RAR archive is treated as
			// occupying volume position 0, the same slot as an obfuscated
			// set's first volume. This means a stray single-volume RAR
			// (e.g. an obfuscated sample) sharing the directory with a
			// genuine obfuscated multi-volume set will collide with it and
			// suppress recovery of the real set -- an intentional
			// false-negative: this stage prefers doing nothing over
			// guessing which candidate is the real first volume.
			volIdx = 0
		}
		if existingPath, ok := byVolume[volIdx]; ok {
			logf(ctx, log, job, slog.LevelWarn,
				"rar_volume_recovery: ambiguous volume %d claimed by both %s and %s -- skipping recovery",
				volIdx, existingPath, p)
			ambiguous = true
			continue
		}
		byVolume[volIdx] = p
	}

	if ambiguous || len(byVolume) == 0 {
		return nil
	}

	base := job.JobID()
	if job.Name() != "" {
		base = job.Name()
	}
	base = fsutil.SanitizeFilename(base, job.Sanitize)

	for volIdx, p := range byVolume {
		newName := fmt.Sprintf("%s.part%03d.rar", base, volIdx+1)
		newPath := filepath.Join(job.DownloadDir, newName)
		if newPath == p {
			continue
		}
		if err := os.Rename(p, newPath); err != nil {
			logf(ctx, log, job, slog.LevelWarn, "rar_volume_recovery: rename %s -> %s failed: %v", p, newPath, err)
			continue
		}
		markRenamed(job, p, newPath)
		logf(ctx, log, job, slog.LevelInfo, "rar_volume_recovery: recovered %s as volume %d -> %s", filepath.Base(p), volIdx, newName)
	}

	return nil
}

func (s *RarVolumeRecoveryStage) logger(job *Job) *slog.Logger {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	return log.With("component", "rar_volume_recovery", "job", job.JobID())
}
