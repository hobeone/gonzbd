package par2

// CRCResult records the outcome of a single file CRC verification.
type CRCResult struct {
	// FileName is the checked file's name on disk, as identification found
	// it — which for a file a previous run relocated includes its
	// subdirectory.
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
	// Unverified is the count of par2 entries that could not be checked
	// against any delivered file's CRC.
	//
	// Two things reach it, and both are now genuine rather than artefacts of
	// name matching: a par2 entry that identification could not match to any
	// file in the directory, and a file identified in the directory that the
	// caller lists no assembled CRC for.
	//
	// What it no longer means is "the names did not line up". Identification
	// runs first and matches by CONTENT, so an entry reaching here was not
	// merely called something else. That distinction is the whole of #492:
	// the doc once claimed "these files exist on disk", two callers read it
	// differently — one as benign, one as damage — and neither reading was
	// safe, because name matching could not tell an obfuscated release from
	// a missing one.
	Unverified int
	// Files holds per-file results for files that were actually checked.
	Files []CRCResult
	// NoCRCFiles lists the names of par2-tracked files that could not
	// be verified because their assembled CRC was 0 (download failures).
	NoCRCFiles []string
	// UnverifiedFiles lists the par2 manifest paths (which may include
	// subdirectory components, e.g. "Screens/foo.jpg") of the entries counted
	// in Unverified.
	UnverifiedFiles []string
}

// AssembledFile represents a downloaded file with its assembled CRC32.
type AssembledFile struct {
	// FileName is the name the caller knows this file by, which is the name
	// it currently has on disk. Assess joins on its basename.
	FileName string
	// CRC32 is the CRC computed during assembly. Zero if unavailable, which
	// is not the same as a CRC of zero — see CRCVerifyResult.NoCRC.
	CRC32 uint32
}

// There is deliberately no size field here, and no size comparison anywhere
// in this package's verification.
//
// One shipped, and it could not fire. Its key was {CRC32, FileSize} with
// exact equality, but the two sides were different quantities: par2's
// FileDesc.FileSize is a file's decoded length, while the value callers had
// to hand came from the NZB — the sum of the yEnc-ENCODED segment sizes,
// measured on a real release at 1.0307x-1.0793x the decoded length. No
// production call site could make them equal, and its only test supplied the
// size from par2's own descriptor, which is why nothing caught it.
//
// Identify does compare sizes, and correctly: both of its sides are the same
// quantity — par2's recorded length against the file's actual length from
// os.DirEntry.Info(). Do not "unify" the two without reading both.
