package postproc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/par2"
)

// Par2CleanupStage records .par2 files and par2-created backup files from the
// job's download directory for deletion after FinalizeStage moves the job to
// FinalDir (#768). It runs after unpack and only proceeds when both repair and
// unpack succeeded (no ParError, no UnpackError) and every par2 set quickcheck
// deferred until after unpack has been verified against the extracted files
// (Job.deferredPar2Verified). Keeping the files on disk until finalize ensures
// a crash before finalize can safely rerun post-processing.
type Par2CleanupStage struct {
	// cleanup is set atomically so SetCleanup can be called from any goroutine
	// (e.g. the API handler) while a job may be running in the postproc worker.
	cleanup   atomic.Bool
	ParseOpts par2.ParseOptions
	Log       *slog.Logger
}

// NewPar2CleanupStage constructs a Par2CleanupStage.
func NewPar2CleanupStage(cleanup bool) *Par2CleanupStage {
	s := &Par2CleanupStage{}
	s.cleanup.Store(cleanup)
	return s
}

// SetCleanup enables or disables par2 file deletion at runtime without
// requiring a server restart. Thread-safe; may be called from any goroutine.
func (s *Par2CleanupStage) SetCleanup(enabled bool) { s.cleanup.Store(enabled) }

// CleanupEnabled reports whether par2 file deletion is active. Thread-safe.
func (s *Par2CleanupStage) CleanupEnabled() bool { return s.cleanup.Load() }

// Name implements Stage.
func (*Par2CleanupStage) Name() string { return "par2_cleanup" }

// Run records all par2 files and par2 backup files in job.DownloadDir for
// deletion at finalize (#768). Skipped when Cleanup is false, when repair or
// unpack has failed, or while a deferred par2 set is unverified.
func (s *Par2CleanupStage) Run(ctx context.Context, job *Job) error {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "par2_cleanup", "job", job.JobID())

	if !s.cleanup.Load() {
		return nil
	}

	if job.ParError {
		logf(ctx, log, job, slog.LevelInfo, "Keeping par2 files (repair failed)")
		return nil
	}
	if job.UnpackError {
		logf(ctx, log, job, slog.LevelInfo, "Keeping par2 files (unpack failed)")
		return nil
	}
	if len(job.deferredPar2Sets) > 0 && !job.deferredPar2Verified {
		// A deferred set protects extracted files that extracted_repair has
		// not verified against it, and the archive's own checksums have gaps
		// (see ExtractedRepairStage). It is the one thing that still can.
		logf(ctx, log, job, slog.LevelInfo, "Keeping par2 files (they protect the extracted files, which par2 has not verified)")
		return nil
	}

	sets, err := par2.FindPar2Files(job.DownloadDir, s.ParseOpts)
	if err != nil {
		log.Warn("par2 cleanup: failed to scan for par2 files", "err", err)
		return nil
	}

	var cleaned int
	for _, set := range sets {
		if set.MainFile != "" {
			if _, err := os.Lstat(set.MainFile); err == nil && job.recordPendingDeletion(set.MainFile) {
				line := "Queued par2 file for deletion at finalize: " + filepath.Base(set.MainFile)
				job.OutputLines = append(job.OutputLines, "[par2_cleanup] "+line)
				if job.OnOutput != nil {
					job.OnOutput("par2_cleanup", line)
				}
				cleaned++
			}
		}
		for _, ef := range set.ExtraFiles {
			if _, err := os.Lstat(ef); err == nil && job.recordPendingDeletion(ef) {
				line := "Queued par2 file for deletion at finalize: " + filepath.Base(ef)
				job.OutputLines = append(job.OutputLines, "[par2_cleanup] "+line)
				if job.OnOutput != nil {
					job.OnOutput("par2_cleanup", line)
				}
				cleaned++
			}
		}
	}
	if cleaned > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Queued %d par2 file(s) for deletion at finalize", cleaned)
	}

	// Par2 repair creates backup copies of damaged files by appending
	// ".1", ".2" etc. (e.g. "movie.part01.rar" → "movie.part01.rar.1") and
	// records them in job.par2Backups. Also scan for "<target>.<N>" backups
	// alongside an existing par2-verified "<target>" (verified intact by the
	// stage gates above) whose size is at most "<target>"'s par2 description
	// size and that share at least one aligned chunk with "<target>", so a
	// crash between repair and finalize still cleans them up on rerun when
	// job.par2Backups is empty without touching unrelated "<target>.<N>"
	// files. Promote them to pendingDeletions so subsequent stages
	// (deobfuscate, unwanted_cleanup, extension_cleanup) skip them and
	// finalize deletes them after moving the job.
	var backupCleaned int
	candidates := append([]string(nil), job.par2Backups...)
	candidates = append(candidates, findVerifiedPar2Backups(job.DownloadDir, sets, job.Sanitize, s.ParseOpts, log)...)
	for _, backup := range candidates {
		abs := filepath.Join(job.DownloadDir, backup)
		if _, err := os.Lstat(abs); err == nil && job.recordPendingDeletion(abs) {
			line := "Queued par2 backup file for deletion at finalize: " + filepath.Base(backup)
			job.OutputLines = append(job.OutputLines, "[par2_cleanup] "+line)
			if job.OnOutput != nil {
				job.OnOutput("par2_cleanup", line)
			}
			backupCleaned++
		}
	}
	if backupCleaned > 0 {
		logf(ctx, log, job, slog.LevelInfo, "Queued %d par2 backup file(s) for deletion at finalize", backupCleaned)
	}

	return nil
}

type par2TargetSpec struct {
	hash16k [16]byte
	size    int64
}

const par2BackupSampleChunk int64 = 16 * 1024

// findVerifiedPar2Backups scans dir for non-member "<target>.<N>" files where
// "<target>" exists as a regular file whose 16 KiB MD5 matches its par2
// FileDescription, the candidate's byte size is in (0, FileSize], and the
// candidate shares at least one aligned chunk with "<target>" (#768).
func findVerifiedPar2Backups(dir string, sets []par2.Set, opts fsutil.SanitizeOptions, parseOpts par2.ParseOptions, log *slog.Logger) []string {
	if len(sets) == 0 {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil
	}
	defer root.Close() //nolint:errcheck // read-only close

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	knownTargets := make(map[string]par2TargetSpec)
	for _, set := range sets {
		pf := set.ParseFile()
		if pf == "" {
			continue
		}
		descs, derr := par2.ParseFileDescriptions(pf, parseOpts)
		if derr != nil {
			log.Warn("par2 cleanup: failed to parse par2 descriptions", "path", pf, "err", derr)
			continue
		}
		for _, d := range descs {
			relDst := fsutil.SanitizeFilename(d.FileName, opts)
			knownTargets[relDst] = par2TargetSpec{
				hash16k: d.Hash16k,
				size:    int64(d.FileSize), //nolint:gosec // G115: file size fits in int64
			}
		}
	}
	if len(knownTargets) == 0 {
		return nil
	}

	var backups []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if _, isTarget := knownTargets[name]; isTarget {
			continue
		}
		lastDot := strings.LastIndexByte(name, '.')
		if lastDot <= 0 {
			continue
		}
		relDst := name[:lastDot]
		spec, isTarget := knownTargets[relDst]
		if !isTarget || !par2.IsBackupNamedFor(root, name, relDst, spec.hash16k) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.Size() <= 0 || info.Size() > spec.size {
			continue
		}
		if sharesAlignedChunk(root, name, relDst, info.Size()) {
			backups = append(backups, name)
		}
	}
	return backups
}

// sharesAlignedChunk reports whether candidateName (of size candSize > 0) and
// targetName share at least one byte-identical aligned chunk (up to 16 KiB) at
// the start, second chunk, middle, or tail of candidateName (#768).
func sharesAlignedChunk(root *os.Root, candidateName, targetName string, candSize int64) bool {
	candFile, err := root.Open(candidateName)
	if err != nil {
		return false
	}
	defer candFile.Close() //nolint:errcheck // read-only close

	targetFile, err := root.Open(targetName)
	if err != nil {
		return false
	}
	defer targetFile.Close() //nolint:errcheck // read-only close

	offsets := [4]int64{
		0,
		par2BackupSampleChunk,
		((candSize / 2) / par2BackupSampleChunk) * par2BackupSampleChunk,
		max(candSize-par2BackupSampleChunk, par2BackupSampleChunk),
	}
	var candBuf, targetBuf [par2BackupSampleChunk]byte
	for i, off := range offsets {
		if off < 0 || off >= candSize {
			continue
		}
		if i > 0 && slices.Contains(offsets[:i], off) {
			continue
		}
		n := min(par2BackupSampleChunk, candSize-off)
		if _, err := io.ReadFull(io.NewSectionReader(candFile, off, n), candBuf[:n]); err != nil {
			continue
		}
		if _, err := io.ReadFull(io.NewSectionReader(targetFile, off, n), targetBuf[:n]); err != nil {
			continue
		}
		if bytes.Equal(candBuf[:n], targetBuf[:n]) {
			return true
		}
	}
	return false
}
