package par2

import (
	"fmt"
	"log/slog"
	"path/filepath"
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
	// Skipped is the count of files that could not be verified (no
	// assembled CRC or no par2 entry found).
	Skipped int
	// Files holds per-file results for files that were actually checked.
	Files []CRCResult
}

// AssembledFile represents a downloaded file with its assembled CRC32.
type AssembledFile struct {
	// FileName is the flat filename on disk (the Subject from the NZB).
	FileName string
	// CRC32 is the CRC computed during assembly. Zero if unavailable.
	CRC32 uint32
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
func VerifyCRCs(files []AssembledFile, sets []Set, log *slog.Logger) CRCVerifyResult {
	if log == nil {
		log = slog.Default()
	}

	// Build a lookup from basename → par2 FileDesc (with CRC).
	// We use basename because downloaded files are flat.
	type par2Entry struct {
		desc     FileDesc
		parFile  string // which par2 file this came from
		consumed bool
	}
	par2Index := make(map[string]*par2Entry) // basename → entry

	for _, set := range sets {
		parFile := set.MainFile
		if parFile == "" && len(set.ExtraFiles) > 0 {
			parFile = set.ExtraFiles[0]
		}
		if parFile == "" {
			continue
		}

		descs, err := ParseFileDescriptions(parFile)
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
		return CRCVerifyResult{Skipped: len(files)}
	}

	log.Info("verifycrc: par2 manifest loaded",
		"entries", len(par2Index),
		"files_to_check", len(files))

	var result CRCVerifyResult

	for _, af := range files {
		// Skip files with no assembled CRC (UU-encoded or failed).
		if af.CRC32 == 0 {
			log.Debug("verifycrc: no assembled CRC, skipping",
				"file", af.FileName)
			result.Skipped++
			continue
		}

		entry, ok := par2Index[af.FileName]
		if !ok {
			// File not in par2 manifest — this is normal for par2 files
			// themselves, NZB files, etc.
			log.Debug("verifycrc: file not in par2 manifest, skipping",
				"file", af.FileName)
			result.Skipped++
			continue
		}

		if entry.desc.FileCRC32 == 0 {
			// Par2 file didn't have IFSC data for this file.
			log.Debug("verifycrc: par2 has no CRC for file, skipping",
				"file", af.FileName)
			result.Skipped++
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

	log.Info("verifycrc: complete",
		"checked", result.Checked,
		"matched", result.Matched,
		"mismatched", result.Mismatched,
		"skipped", result.Skipped)

	return result
}
