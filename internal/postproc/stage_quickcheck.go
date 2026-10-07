package postproc

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// QuickCheckStage relocates flat-downloaded files into the subdirectory structure expected by par2.
type QuickCheckStage struct {
	// toggle provides the thread-safe SetEnabled/enabled flag.
	toggle
	// ParseOpts defines safety limits for PAR2 parsing.
	ParseOpts par2.ParseOptions
	// Log is the component-scoped logger for this stage.
	Log *slog.Logger
	// Unpack is the pipeline's unpack stage, read to learn whether it will
	// run for a job before repair is allowed to defer to it. Nil means it
	// will not; see unpackWillRun.
	Unpack *UnpackStage
}

// NewQuickCheckStage constructs a QuickCheckStage with default settings.
func NewQuickCheckStage() *QuickCheckStage { return &QuickCheckStage{} }

// Name returns the stage identifier.
func (*QuickCheckStage) Name() string { return "quickcheck" }

// Run finds par2 sets and relocates flat files into their par2-specified
// subdirectory paths. After relocation, it verifies file integrity by
// comparing assembled CRC32 values (computed during download) against
// par2 manifest CRC32 values. Errors are non-fatal: par2 repair will
// independently report any files it cannot find or that are corrupted.
func (q *QuickCheckStage) Run(ctx context.Context, job *Job) error {
	log := q.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "quickcheck", "job", job.JobID())

	if !q.enabled() {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Disabled — par2 repair will run the full verify/repair step")
		return nil
	}

	logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Scanning for par2 files in %s", job.DownloadDir)

	sets, err := par2.FindPar2Files(job.DownloadDir, q.ParseOpts)
	if err != nil {
		// Inconclusive, not NotRun: the scan failed, so whether this job has
		// par2 sets is unknown. Claiming there was nothing to check would let
		// the repair stage's DirectUnpack shortcut skip par2 on the strength
		// of a question that was never answered.
		job.QuickCheck = QuickCheckInconclusive
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Failed to find par2 files: %v", err)
		return nil // non-fatal
	}
	if len(sets) == 0 {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] No par2 files found — skipping subdirectory relocation and CRC verification")
		return nil
	}

	// Past this line the job has par2 sets, so QuickCheckNotRun — "there was
	// nothing to verify" — is no longer a true thing to leave behind. Claim
	// nothing by default and narrow to Clean, Damaged or Unidentified only
	// where the work was actually done (#314).
	//
	// This inverts which state is free. The zero value used to be the
	// permissive one, so every early return that forgot to assign handed the
	// repair stage consent to skip par2 — and one did: recordVerdict's opening
	// guard, reachable only from here, so only ever for a job that has par2
	// sets. Now the conservative state is the one you get for free and the
	// permissive ones require saying so, which makes the next early return
	// added below fail safe by construction rather than by review.
	job.QuickCheck = QuickCheckInconclusive

	logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Found %d par2 set(s), checking for subdirectory entries", len(sets))

	// One assessment, taken before anything moves, answering identification,
	// verification and "what would move" together.
	//
	// This stage used to relocate first and verify afterwards, which meant
	// verification matched par2 entries against names the relocation had just
	// invalidated. The compensation was a local rename map applied to the
	// names before comparing them — correct, but a second enforcement point
	// for an ordering the download path enforced separately (#494). Renames
	// now come out of the same call as the verdict, so there is no ordering
	// left for this stage to get right.
	a, err := q.assess(job, sets, log)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Error: %v", err)
		return nil // non-fatal; the outcome set above already says why
	}

	renames := par2.ApplyRenames(job.DownloadDir, a, log)
	if len(renames) > 0 {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] Relocated %d file(s) into subdirectories", len(renames))
		for _, r := range renames {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] %s → %s", r.From, r.To))
		}
	} else {
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] No files needed subdirectory relocation")
	}

	return q.recordVerdict(ctx, log, job, sets, a)
}

// assess builds the assembled-file list from the queue and hands it to
// par2.Assess. Split out so the renames applied above and the verdict read
// below come from ONE call rather than two — which is the invariant this whole
// change exists for, not a tidiness preference.
//
// A job with no readable manifest still gets assessed, with no CRCs. That is
// deliberate: identification and the relocations it implies do not depend on
// the queue at all, and a job whose manifest cannot be read still wants its
// files put where par2 expects them. recordVerdict then declines to claim
// anything, which is what leaves QuickCheckInconclusive standing.
func (q *QuickCheckStage) assess(job *Job, sets []par2.Set, log *slog.Logger) (par2.Assessment, error) {
	var files []par2.AssembledFile
	if job.HasRecord() && job.NumFiles() > 0 {
		if m, err := job.Manifest(); err == nil {
			p := job.Progress()
			files = make([]par2.AssembledFile, m.NumFiles())
			for fi := range m.NumFiles() {
				name := m.FileSubject(fi)
				if fn := p.FileFilename(fi); fn != "" {
					name = fn
				}
				files[fi] = par2.AssembledFile{
					FileName: name,
					CRC32:    p.FileAssembledCRC32(fi),
				}
			}
		}
	}
	return par2.AssessWithOptions(job.DownloadDir, sets, files, log, q.ParseOpts)
}

func (q *QuickCheckStage) recordVerdict(ctx context.Context, log *slog.Logger, job *Job, sets []par2.Set, a par2.Assessment) error {
	// No manifest, or one describing no files, so there are no expected CRCs
	// to compare the par2 sets against. The caller has already established
	// that sets is non-empty — it returns at len(sets) == 0 — so this leaves
	// QuickCheckInconclusive, set there. It used to leave the zero value,
	// which told the repair stage this job had nothing worth verifying while
	// its par2 sets went unchecked (#314).
	if !job.HasRecord() || job.NumFiles() == 0 {
		logf(ctx, log, job, slog.LevelWarn,
			"[quickcheck] No manifest files to verify against, though %d par2 set(s) are present — par2 repair will run", len(sets))
		return nil
	}

	// Unlike the file listing, this must not degrade quietly: a job whose
	// manifest is unreadable would otherwise be reported as CRC-verified
	// having been checked against nothing. Returning an error records the
	// failure in the stage log and surfaces it in the history entry; the
	// runner deliberately does not abort the pipeline on a stage error, so
	// par2 repair still gets its turn.
	//
	// Leaving Inconclusive in place is what makes that last clause true. The
	// error return alone protects this stage's own claim, but the repair stage
	// reads the outcome, not the error — and under the old boolean pair
	// "could not verify" was indistinguishable from "had nothing to verify",
	// so DirectUnpack's success would skip par2 for a job nothing had
	// checked (#294).
	if _, mErr := job.Manifest(); mErr != nil {
		return fmt.Errorf("quickcheck: cannot verify CRCs without the manifest: %w", mErr)
	}

	// Sets whose files do not exist until unpack runs are verified after it,
	// by extracted_repair, so the verdict below is read over the other sets
	// only. Nothing is re-matched: CRCExcluding reads the same pre-rename
	// identification, and the CRCs were compared against the entries
	// identification proved each file to be, before any of the moves above
	// happened.
	deferred := q.deferredSets(ctx, log, job, a.ID)
	job.DeferredPar2Sets = deferred
	skip := make(map[string]bool, len(deferred))
	for _, name := range deferred {
		skip[name] = true
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] par2 set %q protects files a delivered archive holds — it is verified after unpack, not before", name)
	}
	crcResult := a.CRCExcluding(skip, log)
	unverifiable := crcResult.NoCRC + crcResult.Unverified + crcResult.Mismatched

	if crcResult.Checked > 0 {
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] CRC verification: %d/%d par2-tracked files verified OK",
			crcResult.Matched, crcResult.Checked+crcResult.NoCRC)

		for _, f := range crcResult.Files {
			if f.Match {
				job.OutputLines = append(job.OutputLines,
					fmt.Sprintf("[quickcheck] ✓ %s: CRC verified (%08x)", f.FileName, f.AssembledCRC))
			}
		}

		if crcResult.Mismatched > 0 {
			logf(ctx, log, job, slog.LevelWarn,
				"[quickcheck] CRC MISMATCH detected — %d file(s) corrupted",
				crcResult.Mismatched)
			for _, f := range crcResult.Files {
				if !f.Match {
					job.OutputLines = append(job.OutputLines,
						fmt.Sprintf("[quickcheck] ✗ %s: CRC mismatch (assembled=%08x par2=%08x)",
							f.FileName, f.AssembledCRC, f.Par2CRC))
				}
			}
		}
	}

	if crcResult.NoCRC > 0 {
		for _, name := range crcResult.NoCRCFiles {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] ⚠ %s: CRC unavailable, verifying with par2", name))
		}
	}

	if crcResult.Unverified > 0 {
		// "could not be verified", not "not found by name". Nothing is
		// matched by name any more — identification is by content — and
		// Unverified now covers two causes that this aggregate cannot tell
		// apart: a par2 entry no delivered file was shown to be, and an
		// identified file the queue supplied no CRC for. Naming either one
		// specifically would be false half the time, and an operator reading
		// "not found by name" would go looking for a filename problem that
		// does not exist.
		logf(ctx, log, job, slog.LevelWarn,
			"[quickcheck] %d par2-tracked file(s) could not be verified",
			crcResult.Unverified)
		for _, name := range crcResult.UnverifiedFiles {
			job.OutputLines = append(job.OutputLines,
				fmt.Sprintf("[quickcheck] ⚠ %s: par2-tracked file could not be verified", name))
		}
	}

	switch {
	case len(deferred) > 0 && len(deferred) == len(setsWithEntries(a.ID)):
		job.QuickCheck = QuickCheckUnidentified
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] Every par2 set protects files a delivered RAR/7z archive holds — repair is skipped, and par2 "+
				"verifies the extracted files after unpack")
	case unverifiable > 0:
		job.QuickCheck = QuickCheckDamaged
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] %d/%d par2-tracked files verified OK, %d file(s) need par2 verification — repair stage will run",
			crcResult.Matched, crcResult.Matched+unverifiable, unverifiable)
	case crcResult.Checked > 0:
		job.QuickCheck = QuickCheckClean
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] All %d par2-tracked files verified OK — skipping par2 repair",
			crcResult.Matched)
	default:
		// Par2 sets were found but no assembled CRC was available to compare
		// against any of them, so nothing was actually verified. Stays at the
		// Inconclusive the caller set, which also keeps the pre-enum
		// behaviour: the old pair recorded Ran-and-not-Passed here, forcing
		// repair.
		logf(ctx, log, job, slog.LevelInfo, "[quickcheck] No CRC data available — par2 repair will run")
	}
	return nil
}

// deferredSets returns, sorted, the par2 sets whose verification waits for
// unpack: sets judged to protect files a delivered archive will extract
// (Layout B), which a repair before unpack has nothing to verify against and
// whose failure would skip the unpack that produces them. It is a heuristic,
// judged per set on the entries identification did not account for, and a
// set that fails any condition is repaired before unpack as usual.
//
//  1. Unpack will run for this job. Deferring hands the set to
//     extracted_repair, which runs only after unpack, so a job unpack will
//     skip — PP below PPUnpack, or the stage disabled — defers nothing.
//  2. The set has an entry no delivered file was identified as. A set with
//     every entry accounted for has everything it protects on disk now.
//     Entries it did identify — a sidecar .nfo it also protects — are
//     verified with the rest after unpack.
//  3. None of those entries is itself named as an archive (unpack.Classify).
//     par2 names are the poster's real names, so an entry called
//     "Release.part01.rar" says the set protects archives — Layout A, where
//     an obfuscated volume damaged in its first 16 KB matches nothing and
//     repair is exactly what it needs.
//  4. Each of those entries' base name is a member of a RAR or 7z archive in
//     the directory (heldArchiveMembers). An archive the entries do not name
//     says nothing about them.
func (q *QuickCheckStage) deferredSets(ctx context.Context, log *slog.Logger, job *Job, id par2.Identification) []string {
	unaccounted := make(map[string][]par2.FileDesc)
	for _, fd := range id.Unaccounted {
		unaccounted[fd.Set] = append(unaccounted[fd.Set], fd)
	}
	if len(unaccounted) == 0 {
		return nil
	}
	if !q.unpackWillRun(job) {
		logf(ctx, log, job, slog.LevelInfo,
			"[quickcheck] par2 names files nothing delivered matches, and unpack will not run for this job — repair will run")
		return nil
	}

	var candidates []string
	var entries []par2.FileDesc
	for _, set := range slices.Sorted(maps.Keys(unaccounted)) {
		fds := unaccounted[set]
		named := slices.IndexFunc(fds, func(fd par2.FileDesc) bool {
			return unpack.Classify(unpack.MemberBaseName(fd.FileName)) != unpack.UnknownArchive
		})
		if named >= 0 {
			logf(ctx, log, job, slog.LevelInfo,
				"[quickcheck] par2 set %q protects an archive (%s) that nothing delivered matches — repair will run",
				set, fds[named].FileName)
			continue
		}
		candidates = append(candidates, set)
		entries = append(entries, fds...)
	}
	if len(candidates) == 0 {
		return nil
	}
	held, ok := heldArchiveMembers(ctx, log, job, entries)
	if !ok {
		return nil
	}

	var deferred []string
	for _, set := range candidates {
		missing := 0
		for _, fd := range unaccounted[set] {
			if !held[unpack.MemberBaseName(fd.FileName)] {
				missing++
			}
		}
		if missing > 0 {
			logf(ctx, log, job, slog.LevelInfo,
				"[quickcheck] par2 set %q: %d of %d unmatched file(s) are named in no delivered archive — repair will run",
				set, missing, len(unaccounted[set]))
			continue
		}
		deferred = append(deferred, set)
	}
	return deferred
}

// setsWithEntries returns the par2 sets that contributed any entry to id,
// identified or not. A set whose index could not be read contributes none.
func setsWithEntries(id par2.Identification) map[string]bool {
	sets := make(map[string]bool)
	for _, f := range id.Files {
		sets[f.Desc.Set] = true
	}
	for _, fd := range id.Unaccounted {
		sets[fd.Set] = true
	}
	return sets
}

// unpackWillRun reports whether the unpack stage will run for job: it is
// wired, enabled, and not skipped by the job's PP level (shouldSkipForPP, the
// rule processJob applies). Unwired counts as not running.
func (q *QuickCheckStage) unpackWillRun(job *Job) bool {
	return q.Unpack != nil && q.Unpack.IsEnabled() && !shouldSkipForPP(q.Unpack.Name(), job.PP)
}

// heldArchiveMembers returns which of entries' base names are members of some
// RAR or 7z archive in the download directory. Archives are found by name
// (unpack.Scan), so an obfuscated volume that rar_volume_recovery would rename
// later is not seen, and members are read from headers only
// (unpack.MemberBaseNames, which for RAR reads the first volume, and lists
// only RAR and 7z). ok is false after a scan or listing error, which the
// caller reads as nothing held.
func heldArchiveMembers(ctx context.Context, log *slog.Logger, job *Job, entries []par2.FileDesc) (held map[string]bool, ok bool) {
	archives, err := unpack.Scan(job.DownloadDir)
	if err != nil {
		logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Archive scan failed: %v — repair will run", err)
		return nil, false
	}
	want := make(map[string]bool, len(entries))
	for _, fd := range entries {
		want[unpack.MemberBaseName(fd.FileName)] = true
	}
	held = make(map[string]bool, len(want))
	for _, a := range archives {
		if a.Type != unpack.RarArchive && a.Type != unpack.SevenZipArchive {
			continue
		}
		found, err := unpack.MemberBaseNames(a, want)
		if err != nil {
			logf(ctx, log, job, slog.LevelWarn, "[quickcheck] Cannot list archive members: %v — repair will run", err)
			return nil, false
		}
		for name := range found {
			held[name] = true
		}
	}
	return held, true
}
