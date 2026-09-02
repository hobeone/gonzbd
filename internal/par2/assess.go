package par2

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
)

// Assessment is the whole answer to "what is in this directory, and is it
// intact" — identification, verification, and the moves that would put every
// file at the path par2 records.
//
// # Why these three arrive together
//
// They used to be three calls, and the ORDER between them was the defect.
// Every caller did the same thing:
//
//	relocate the files  ->  verify by matching names
//
// The relocation invalidates exactly the names verification matches on, so
// each caller needed its own way to survive its own rename. Four shipped, all
// different, all for one root cause (#492, #494): re-reading a snapshot the
// rename had made stale; a pass so an already-relocated file could be seen at
// all; reducing a path to a basename so it could still be recorded; and an
// in-memory remap where there was no writer to record through.
//
// Assess inverts it:
//
//	identify  ->  join  ->  verify  ->  hand back the moves, unapplied
//
// The join from delivered file to par2 entry happens while the names are
// still the ones on disk, so staleness is not fixed here so much as made
// unreachable. Renames cannot be obtained without a verdict computed from
// pre-rename state, because they come out of the same call — which is the
// point of the type. Applying them is ApplyRenames, and it is deliberately a
// separate, second act.
//
// This is the owner-model answer AGENTS.md Standing Design Rule 2 asks for:
// an ordering every caller must remember is a check, and it had already been
// forgotten four times. There is now nowhere else to compute this.
type Assessment struct {
	// ID is which par2 entry each delivered file was shown to be.
	ID Identification
	// CRC is whether the files so identified are intact.
	CRC CRCVerifyResult
	// Renames are the moves that would put each identified file at its par2
	// path. NOT applied — pass them to ApplyRenames.
	//
	// Only identifications where NeedsRename() is true appear here, so an
	// ordinary flat job yields none rather than a self-move per file.
	Renames []Rename
}

// Assess identifies the files in dir against the par2 sets, verifies the
// identified ones against the assembled CRCs the caller supplies, and reports
// the renames that would relocate them — all from the directory's current,
// pre-rename state.
//
// files carries what only the caller can know: the CRC32 computed for each
// delivered file during download. Assess never reads a payload to compute one;
// that value is free at download time and expensive here.
//
// It performs no writes. See ApplyRenames.
func Assess(dir string, sets []Set, files []AssembledFile, log *slog.Logger) (Assessment, error) {
	return AssessWithOptions(dir, sets, files, log, DefaultParseOptions())
}

// AssessWithOptions is Assess with explicit par2 parse options.
func AssessWithOptions(dir string, sets []Set, files []AssembledFile, log *slog.Logger, opts ParseOptions) (Assessment, error) {
	if log == nil {
		log = slog.Default()
	}

	id, err := IdentifyWithOptions(dir, sets, log, opts)
	if err != nil {
		return Assessment{}, err
	}

	a := Assessment{ID: id, CRC: verifyIdentified(id, files, log)}
	for _, f := range id.Files {
		if !f.NeedsRename() {
			continue
		}
		a.Renames = append(a.Renames, Rename{From: f.OnDisk, To: f.Desc.FileName})
	}

	log.Info("assess: complete",
		"identified", len(id.Files),
		"unaccounted", len(id.Unaccounted),
		"matched", a.CRC.Matched,
		"mismatched", a.CRC.Mismatched,
		"nocrc", a.CRC.NoCRC,
		"unverified", a.CRC.Unverified,
		"renames", len(a.Renames))

	return a, nil
}

// verifyIdentified compares each identified file's assembled CRC32 against the
// CRC32 par2 recorded for the entry it was identified AS.
//
// # The join key is the basename, and that is a decision
//
// Identification answers in terms of the file on disk; the caller's CRCs are
// keyed by the name it knows the file by. Those are the same string in the
// ordinary case, and differ in exactly one: a file a PREVIOUS run already
// relocated is identified as "Screens/shot.jpg" while the caller still knows
// it as "shot.jpg". Joining on the full path would miss it and report an
// intact file unverified.
//
// Basename introduces no ambiguity that was not already there. The pass this
// replaces keyed its par2 index "basename -> entry", so two par2 entries
// sharing a basename have always been indistinguishable to verification. What
// changes is only where that assumption lives: it is now this one line rather
// than an index built on every call.
//
// No name is matched against the par2 MANIFEST here. That is the pass this
// design removes: it is what could not survive obfuscation, and it is what
// Identify already did properly, by content.
func verifyIdentified(id Identification, files []AssembledFile, log *slog.Logger) CRCVerifyResult {
	if log == nil {
		log = slog.Default()
	}

	crcOf := make(map[string]uint32, len(files))
	seen := make(map[string]bool, len(files))
	for _, af := range files {
		key := joinKey(af.FileName)
		crcOf[key] = af.CRC32
		seen[key] = false
	}

	var r CRCVerifyResult
	for _, f := range id.Files {
		key := joinKey(f.OnDisk)
		crc, delivered := crcOf[key]
		if !delivered {
			// Identified against the directory, but the caller lists no
			// delivered file under that name. par2 protects something this
			// job did not download — an extracted file, or a sidecar another
			// job left in the directory — so there is no assembled CRC that
			// could speak to it.
			r.Unverified++
			r.UnverifiedFiles = append(r.UnverifiedFiles, f.Desc.FileName)
			log.Debug("assess: identified a file the caller does not list",
				"file", f.OnDisk, "par2path", f.Desc.FileName)
			continue
		}
		seen[key] = true

		switch {
		case crc == 0:
			// R23's "unavailable", not a CRC of zero. A failed download is
			// one cause; so is a resumed file that is perfectly intact, whose
			// earlier articles were never re-fetched and so contribute
			// nothing to combine. Whether it is damaged is par2's question.
			r.NoCRC++
			r.NoCRCFiles = append(r.NoCRCFiles, f.OnDisk)
			log.Warn("assess: par2-tracked file has no assembled CRC to check", "file", f.OnDisk)
		case f.Desc.FileCRC32 == 0:
			// The set carries no IFSC data for this entry, so par2 recorded
			// no whole-file CRC. Equally unverifiable, and equally par2's
			// question.
			r.NoCRC++
			r.NoCRCFiles = append(r.NoCRCFiles, f.OnDisk)
			log.Warn("assess: par2 has no CRC data for file (no IFSC slices)", "file", f.OnDisk)
		default:
			match := crc == f.Desc.FileCRC32
			r.Files = append(r.Files, CRCResult{
				FileName:     f.OnDisk,
				AssembledCRC: crc,
				Par2CRC:      f.Desc.FileCRC32,
				Match:        match,
				Par2FileName: f.Desc.FileName,
			})
			r.Checked++
			if match {
				r.Matched++
				log.Info("assess: CRC match", "file", f.OnDisk, "crc32", fmt.Sprintf("%08x", crc))
			} else {
				r.Mismatched++
				log.Warn("assess: CRC MISMATCH", "file", f.OnDisk,
					"assembled", fmt.Sprintf("%08x", crc),
					"par2", fmt.Sprintf("%08x", f.Desc.FileCRC32))
			}
		}
	}

	// Entries no delivered file was shown to be. This is the count the fetch
	// decision reads, and it now means what it says: identification is by
	// content, so an entry reaching here was not merely named differently.
	for _, fd := range id.Unaccounted {
		r.Unverified++
		r.UnverifiedFiles = append(r.UnverifiedFiles, fd.FileName)
	}

	// Delivered files that are not part of any par2 set. Normal and benign:
	// the .par2 files themselves, .nfo, .sfv.
	for _, matched := range seen {
		if !matched {
			r.NotInPar2++
		}
	}

	// Sorted because these two lists are user-facing — they are interpolated
	// into the reason a job gives for fetching recovery volumes, and into the
	// post-processing log.
	//
	// Determinism is no longer what the sort is FOR, and that is a change
	// worth stating. The pass this replaces indexed par2 by basename in a map
	// and sorted to escape its iteration order; both of these are now built
	// by walking slices (id.Files, then id.Unaccounted), so their order is
	// already fixed. Sorting is now purely for the reader.
	slices.Sort(r.NoCRCFiles)
	slices.Sort(r.UnverifiedFiles)

	return r
}

// joinKey reduces a name to the form both sides of the identification/CRC
// join agree on. See verifyIdentified for why this is the basename.
func joinKey(name string) string {
	return filepath.Base(filepath.ToSlash(name))
}

// ApplyRenames performs the moves an Assessment reported and returns those
// that succeeded, so a caller tracking file ownership records what actually
// happened rather than what was intended.
//
// It takes the whole Assessment rather than a []Rename, and that is the
// invariant rather than a convenience: the moves are reachable only through a
// value that already carries a verdict computed from pre-rename state, so
// "relocate, then verify" — the ordering behind every defect this replaces —
// cannot be written.
//
// A rename that fails is logged and skipped. The file stays where it was,
// which is consistent with the caller's own record of it, and par2 repair
// will report anything still missing.
func ApplyRenames(dir string, a Assessment, log *slog.Logger) []Rename {
	if log == nil {
		log = slog.Default()
	}
	applied := make([]Rename, 0, len(a.Renames))
	for _, f := range a.ID.Files {
		if !f.NeedsRename() {
			continue
		}
		if relocateFile(dir, f.OnDisk, f.Desc, log) {
			applied = append(applied, Rename{From: f.OnDisk, To: f.Desc.FileName})
		}
	}
	if len(applied) > 0 {
		log.Info("assess: applied renames", "planned", len(a.Renames), "applied", len(applied))
	}
	return applied
}
