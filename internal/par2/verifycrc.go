package par2

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
)

// CRCResult records the outcome of a single file CRC verification.
type CRCResult struct {
	// FileName is the name of the file that was checked (basename).
	FileName string
	// AssembledCRC is the CRC32 computed during download by combining
	// per-article yEnc CRCs in offset order.
	AssembledCRC uint32
	// Par2CRC is the CRC32 from the par2 manifest, reconstructed from
	// IFSC slices.
	Par2CRC uint32
	// Match is true if AssembledCRC == Par2CRC.
	Match bool
	// Par2FileName is the full path from the par2 manifest (may include
	// subdirectory components like "Screens/foo.jpg").
	Par2FileName string
}

// CRCVerifyResult aggregates all CRC verification outcomes.
type CRCVerifyResult struct {
	// Checked is the total number of files that had both assembled CRC
	// and par2 CRC available for comparison.
	Checked int
	// Matched is the count of files whose CRCs matched.
	Matched int
	// Mismatched is the count of files whose CRCs did not match.
	Mismatched int
	// NoCRC is the count of par2-tracked files whose assembled CRC was
	// unavailable. A failed download is one cause, but so is a resumed file
	// that is perfectly intact: the articles an earlier run completed are
	// not re-fetched, so they contribute nothing to the combination and no
	// whole-file CRC can be produced. NoCRC therefore says the file could
	// not be checked, not that it is damaged — which is why par2 has to
	// decide.
	NoCRC int
	// NotInPar2 is the count of assembled files not tracked by par2.
	// This is expected and benign — par2 files, .nfo files, etc.
	NotInPar2 int
	// Unverified is the count of par2 manifest entries that no assembled file
	// matched by name.
	//
	// It says nothing about the filesystem. This function never stats
	// anything: it compares two in-memory lists, so an entry is Unverified
	// when the NZB delivered no file of that name, which is equally true of a
	// file present under a different name and of a file that does not exist.
	//
	// The doc here used to claim "these files exist on disk", and two callers
	// read that differently — one treating Unverified as benign and the other
	// as damage, which is #492. Identify is what actually answers the
	// on-disk question, and it runs before this.
	Unverified int
	// Files holds per-file results for files that were actually checked.
	Files []CRCResult
	// NoCRCFiles lists the names of par2-tracked files that could not
	// be verified because their assembled CRC was 0 (download failures).
	NoCRCFiles []string
	// UnverifiedFiles lists the par2 manifest paths (may include subdirectory
	// components, e.g. "Screens/foo.jpg") of par2-tracked entries that had no
	// corresponding assembled file — a name mismatch between the NZB and the
	// par2 manifest. These are the files counted in Unverified.
	UnverifiedFiles []string
}

// AssembledFile represents a downloaded file with its assembled CRC32.
type AssembledFile struct {
	// FileName is the flat filename on disk (the Subject from the NZB).
	FileName string
	// CRC32 is the CRC computed during assembly. Zero if unavailable.
	CRC32 uint32
	// FileSize is the total file size in bytes from the NZB metadata — the
	// sum of the yEnc-ENCODED segment sizes, which is 1.03x-1.08x the file's
	// decoded length on a real release.
	//
	// Nothing in this file compares it against par2's FileDesc.FileSize, and
	// nothing should: that field is the decoded length, so the two are never
	// equal. A fallback pass that did exactly that shipped here and could
	// never fire; see the note at the end of VerifyCRCsWithOptions.
	FileSize int64
}

type par2Entry struct {
	desc     FileDesc
	parFile  string // which par2 file this came from
	consumed bool
}

// VerifyCRCs compares assembled CRC32 values (computed during download)
// against CRC32 values from par2 file manifests. This is the "quick check"
// verification that can detect corruption without re-reading files from disk.
//
// files is the list of downloaded files with their assembled CRCs.
// sets are the par2 sets found in the download directory.
//
// Returns a summary of verification results. Errors parsing par2 files are
// logged but don't abort — individual files that can't be verified are
// counted as Skipped.
//
//nolint:gocyclo // the name-match pass has many small early-outs
func VerifyCRCs(files []AssembledFile, sets []Set, log *slog.Logger, opts ...ParseOptions) CRCVerifyResult {
	parseOpts := DefaultParseOptions()
	if len(opts) > 0 {
		parseOpts = opts[0]
	}
	return VerifyCRCsWithOptions(files, sets, log, parseOpts)
}

// VerifyCRCsWithOptions compares assembled CRC32 values using specified ParseOptions.
func VerifyCRCsWithOptions(files []AssembledFile, sets []Set, log *slog.Logger, opts ParseOptions) CRCVerifyResult {
	if log == nil {
		log = slog.Default()
	}

	par2Index := make(map[string]*par2Entry) // basename → entry

	for _, set := range sets {
		parFile := set.ParseFile()
		if parFile == "" {
			continue
		}

		descs, err := ParseFileDescriptionsWithOptions(parFile, opts)
		if err != nil {
			log.Warn("verifycrc: failed to parse par2 file",
				"file", filepath.Base(parFile), "err", err)
			continue
		}

		for _, fd := range descs {
			basename := filepath.Base(filepath.ToSlash(fd.FileName))
			if _, exists := par2Index[basename]; !exists {
				par2Index[basename] = &par2Entry{desc: fd, parFile: parFile}
			}
		}
	}

	if len(par2Index) == 0 {
		log.Info("verifycrc: no par2 file descriptions found for CRC verification")
		return CRCVerifyResult{NotInPar2: len(files)}
	}

	log.Info("verifycrc: par2 manifest loaded",
		"entries", len(par2Index),
		"files_to_check", len(files))

	var result CRCVerifyResult

	// Name-based verification pass
	for _, af := range files {
		entry, inPar2 := matchBasename(af, par2Index)
		if !inPar2 {
			entry, inPar2 = matchFlattened(af, par2Index)
		}

		// Files with no assembled CRC: the download failed, or the file was
		// resumed and no whole-file CRC could be computed for it.
		if af.CRC32 == 0 {
			if inPar2 {
				// A par2-tracked file we cannot check from the assembled
				// CRC. Whether it is damaged is unknown here, so par2 is
				// left to answer it.
				entry.consumed = true
				result.NoCRC++
				result.NoCRCFiles = append(result.NoCRCFiles, af.FileName)
				log.Warn("verifycrc: par2-tracked file has no assembled CRC to check",
					"file", af.FileName)
			} else {
				result.NotInPar2++
				log.Debug("verifycrc: non-par2 file has no CRC, ignoring",
					"file", af.FileName)
			}
			continue
		}

		if !inPar2 {
			// File not in par2 manifest — this is normal for par2 files
			// themselves, NZB files, etc.
			result.NotInPar2++
			log.Debug("verifycrc: file not in par2 manifest, ignoring",
				"file", af.FileName)
			continue
		}

		if entry.desc.FileCRC32 == 0 {
			// Par2 file didn't have IFSC data for this file.
			entry.consumed = true
			result.NoCRC++
			result.NoCRCFiles = append(result.NoCRCFiles, af.FileName)
			log.Warn("verifycrc: par2 has no CRC data for file (no IFSC slices)",
				"file", af.FileName)
			continue
		}

		entry.consumed = true
		match := af.CRC32 == entry.desc.FileCRC32

		cr := CRCResult{
			FileName:     af.FileName,
			AssembledCRC: af.CRC32,
			Par2CRC:      entry.desc.FileCRC32,
			Match:        match,
			Par2FileName: entry.desc.FileName,
		}
		result.Files = append(result.Files, cr)
		result.Checked++

		if match {
			result.Matched++
			log.Info("verifycrc: CRC match",
				"file", af.FileName,
				"crc32", fmt.Sprintf("%08x", af.CRC32))
		} else {
			result.Mismatched++
			log.Warn("verifycrc: CRC MISMATCH",
				"file", af.FileName,
				"assembled", fmt.Sprintf("%08x", af.CRC32),
				"par2", fmt.Sprintf("%08x", entry.desc.FileCRC32))
		}
	}

	// There is deliberately no CRC+size fallback here any more.
	//
	// There was one, and it could not fire. Its key was {CRC32, FileSize}
	// with exact size equality, but the two sides are different quantities:
	// par2's FileDesc.FileSize is the file's decoded length, while
	// AssembledFile.FileSize comes from the NZB — the sum of the yEnc-ENCODED
	// segment sizes, measured on a real release at 1.0307x-1.0793x the
	// decoded length. No production call site can make those equal. Its only
	// test supplied FileSize from par2's own descriptor, which is why nothing
	// caught it.
	//
	// Matching an obfuscated file is Identify's job, not this function's.
	// Identify reads the directory, so its own CRC+size pass compares par2's
	// recorded length against os.DirEntry.Info() — the same quantity on both
	// sides — and applyPar2Names corrects the resolved names before this
	// comparison runs. By the time VerifyCRCs is reached, a name that still
	// does not match is a file identification genuinely could not place, and
	// reporting it Unverified is the honest answer rather than a missed one.
	//
	// Restoring a fallback here would need a real on-disk size, which means
	// this function would have to stat files. It does not touch the
	// filesystem, and that is worth keeping.

	// Count par2 entries that had no corresponding assembled file at all.
	// These represent a name mismatch between NZB and par2 manifest.
	// par2Index is a map, so its iteration order is randomized per run; sort
	// the unconsumed basenames first so UnverifiedFiles (and the resulting
	// UI log lines) are in a stable, reproducible order.
	unconsumed := make([]string, 0, len(par2Index))
	for basename, entry := range par2Index {
		if !entry.consumed {
			unconsumed = append(unconsumed, basename)
		}
	}
	slices.Sort(unconsumed)
	for _, basename := range unconsumed {
		entry := par2Index[basename]
		result.Unverified++
		result.UnverifiedFiles = append(result.UnverifiedFiles, entry.desc.FileName)
		log.Warn("verifycrc: par2-tracked file not found in assembled files (name mismatch?)",
			"par2_basename", basename,
			"par2_path", entry.desc.FileName)
	}

	// Total par2-tracked files in the manifest.
	totalPar2 := len(par2Index)
	log.Info("verifycrc: complete",
		"par2_manifest_files", totalPar2,
		"checked", result.Checked,
		"matched", result.Matched,
		"mismatched", result.Mismatched,
		"no_crc", result.NoCRC,
		"unverified", result.Unverified,
		"not_in_par2", result.NotInPar2)

	return result
}

func matchBasename(af AssembledFile, par2Index map[string]*par2Entry) (*par2Entry, bool) {
	entry, ok := par2Index[af.FileName]
	return entry, ok
}

func matchFlattened(af AssembledFile, par2Index map[string]*par2Entry) (*par2Entry, bool) {
	flat := filepath.Base(filepath.ToSlash(af.FileName))
	entry, ok := par2Index[flat]
	return entry, ok
}
