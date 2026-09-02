package par2

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
)

// MatchMethod records how a delivered file was matched to a par2 entry.
type MatchMethod uint8

const (
	// MatchName is a filename match: the file on disk is already called what
	// par2 calls it (comparing basenames, so a par2 entry naming a
	// subdirectory still matches a flat file).
	MatchName MatchMethod = iota
	// MatchFlattenedName is a filename match against the par2 path with its
	// separators replaced by underscores — "Screens/shot.jpg" delivered as
	// "Screens_shot.jpg", which is how some posters flatten a tree.
	MatchFlattenedName
	// MatchHash16k is a content match on the MD5 of the first 16 KB, the
	// identifier par2 records for exactly this purpose. It is what resolves an
	// obfuscated release, where the delivered name carries no relationship to
	// the par2 name.
	MatchHash16k
	// MatchCRC32 is a content match on the whole-file CRC32 and the on-disk
	// size. It costs a full read and finds almost nothing Hash16k does not,
	// since a file whose first 16 KB differ has either different or damaged
	// content and fails a whole-file check too. It earns its place on one
	// case: entries that share a Hash16k, which content cannot otherwise tell
	// apart.
	//
	// Both sides here are the same quantity — par2's recorded CRC32 and
	// length against the file's actual CRC32 and actual length from
	// os.DirEntry.Info(). That is what verifycrc.go's matchCRCSize gets
	// wrong; do not "unify" the two without reading both.
	MatchCRC32
)

func (m MatchMethod) String() string {
	switch m {
	case MatchName:
		return "name"
	case MatchFlattenedName:
		return "flattened-name"
	case MatchHash16k:
		return "hash16k"
	case MatchCRC32:
		return "crc32"
	default:
		return fmt.Sprintf("MatchMethod(%d)", uint8(m))
	}
}

// Identified is one delivered file and the par2 entry it was shown to be.
type Identified struct {
	// OnDisk is the file's current name, relative to the scanned directory.
	OnDisk string
	// Desc is the par2 entry this file is. Desc.FileName is the path par2
	// records, which may contain subdirectory components.
	Desc FileDesc
	// By is how the match was made.
	By MatchMethod
}

// NeedsRename reports whether acting on this identification would move the
// file. It is false for the ordinary case of a correctly-named flat file,
// where par2's path and the on-disk name are already the same string.
//
// Callers that relocate MUST consult this rather than renaming every
// identification: par2 names a flat file exactly what it is already called, so
// an unconditional rename is a self-move for every file in a normal job.
func (i Identified) NeedsRename() bool {
	return filepath.ToSlash(i.Desc.FileName) != filepath.ToSlash(i.OnDisk)
}

// Identification is the outcome of matching a directory's contents against a
// par2 set's file list.
type Identification struct {
	// Files is every delivered file that was matched to a par2 entry.
	Files []Identified
	// Unaccounted is every par2 entry that no delivered file matched. A set
	// with none of these is fully accounted for: every file par2 protects is
	// present, whatever it is currently called.
	Unaccounted []FileDesc
	// Ignored is the delivered files that were not considered, because par2
	// does not protect its own sidecars. See ignoredExtensions.
	Ignored []string
}

// Accounted reports whether every par2 entry was matched to a delivered file.
//
// This is the question the fetch decision asks. It says nothing about whether
// those files are INTACT — Hash16k covers only the first 16 KB, and a name
// match covers no content at all. Verification is a separate step against
// FileDesc.FileCRC32; see the identify-then-verify design note.
func (id Identification) Accounted() bool { return len(id.Unaccounted) == 0 }

// ignoredExtensions are the sidecars a par2 set does not protect and which are
// therefore never candidates for identification.
//
// par2's own files are excluded because a set cannot describe itself. The
// other two mirror SABnzbd's quick_check_ext_ignore (cfg.py: ["nfo", "sfv",
// "srr"]) minus "srr", matching what this package already skipped implicitly
// inside shouldSkipFlatFile before identification was extracted from it.
var ignoredExtensions = []string{".par2", ".sfv", ".nfo"}

func isIgnoredForIdentification(name string) bool {
	return slices.Contains(ignoredExtensions, strings.ToLower(filepath.Ext(name)))
}

// Identify matches the files in dir against the par2 sets' file lists, WITHOUT
// touching the filesystem beyond reading.
//
// It is the single owner of the question "which par2 entry is this file",
// which two callers need for different reasons: the on-demand par2 decision
// asks whether every entry is accounted for before ruling on recovery volumes,
// and quickcheck asks for the mapping so it can rename. Splitting the answer
// from the action is what lets the first caller ask at all — the matchers this
// replaces relocated a file as an inseparable part of matching it, so there
// was no way to consult them for a verdict.
//
// Matching runs in three passes, cheapest first:
//
//  1. Name. A par2 entry claims the file whose basename equals its own, or
//     whose name is its path with separators flattened to underscores.
//  2. Hash16k. Each still-unclaimed file has the MD5 of its first 16 KB
//     computed once and looked up among the still-unclaimed entries. This is
//     what resolves obfuscation, and it is where the useful work happens.
//  3. Whole-file CRC32, for entries carrying IFSC data that pass 2 could not
//     settle — in practice only those sharing a Hash16k with another entry.
//     It reads whole files, so it is guarded by a size check first.
//
// A file is claimed by at most one entry and an entry claims at most one file,
// so no two passes can consume the same file.
func Identify(dir string, sets []Set, log *slog.Logger) (Identification, error) {
	return IdentifyWithOptions(dir, sets, log, ParseOptions{})
}

// IdentifyWithOptions is Identify with explicit par2 parse options.
func IdentifyWithOptions(dir string, sets []Set, log *slog.Logger, opts ParseOptions) (Identification, error) {
	if log == nil {
		log = slog.Default()
	}

	manifest := collectManifestsWithOptions(sets, log, opts)
	if len(manifest) == 0 {
		log.Info("identify: no file descriptions found in any par2 set")
		return Identification{}, nil
	}

	flatFiles, err := scanFlatFiles(dir, log)
	if err != nil {
		return Identification{}, err
	}

	// Candidate names, in a stable order so the result does not depend on map
	// iteration. Two files can only compete for one entry via Hash16k, and
	// which one wins must not vary between runs on identical input.
	candidates := make([]string, 0, len(flatFiles))
	var ignored []string
	for name := range flatFiles {
		if isIgnoredForIdentification(name) {
			ignored = append(ignored, name)
			continue
		}
		candidates = append(candidates, name)
	}
	slices.Sort(candidates)
	slices.Sort(ignored)

	claimedFile := make(map[string]bool, len(candidates))
	claimedEntry := make(map[int]bool, len(manifest))
	id := Identification{Ignored: ignored}

	// Pass 1 — every basename first, and only then every flattened form.
	//
	// The two sweeps must not be merged into one per-entry loop that tries
	// both candidates. A basename match is the stronger evidence, and running
	// them together lets an earlier entry's WEAK match consume a file that a
	// later entry matches exactly: with entries ["a/b.txt", "Sub/a_b.txt"] and
	// "a_b.txt" on disk, the per-entry form gives the file to "a/b.txt" by its
	// flattened name, while "Sub/a_b.txt" matches it by basename.
	claimName := func(ei int, fd FileDesc, name string, by MatchMethod) {
		if _, ok := flatFiles[name]; !ok || claimedFile[name] || claimedEntry[ei] {
			return
		}
		claimedFile[name] = true
		claimedEntry[ei] = true
		id.Files = append(id.Files, Identified{OnDisk: name, Desc: fd, By: by})
	}
	for ei, fd := range manifest {
		claimName(ei, fd, filepath.Base(filepath.ToSlash(fd.FileName)), MatchName)
	}
	for ei, fd := range manifest {
		claimName(ei, fd, strings.ReplaceAll(filepath.ToSlash(fd.FileName), "/", "_"), MatchFlattenedName)
	}

	// Pass 2 — Hash16k over what pass 1 left. Each unclaimed file is hashed at
	// most once; the index holds only entries still seeking a file.
	// A set that describes two files with identical first 16 KB cannot be
	// disambiguated this way, and silently letting one alias the other would
	// hand par2 the wrong name. Both are dropped from the index, so neither
	// is identified here and pass 3 gets its chance.
	//
	// Ambiguity is tracked in its own set rather than by poisoning the index
	// with a sentinel. A sentinel has to be read back on the next duplicate,
	// and the obvious choice — a negative index — panics at manifest[prev] the
	// moment a THIRD entry shares the hash. This shape has no such read.
	hashIndex := make(map[[16]byte]int, len(manifest))
	ambiguous := make(map[[16]byte]bool)
	for ei, fd := range manifest {
		if claimedEntry[ei] {
			continue
		}
		if _, dup := hashIndex[fd.Hash16k]; dup {
			ambiguous[fd.Hash16k] = true
			continue
		}
		hashIndex[fd.Hash16k] = ei
	}
	for h := range ambiguous {
		log.Warn("identify: par2 entries share a Hash16k; none can be identified by first-16KB content",
			"par2path", manifest[hashIndex[h]].FileName)
		delete(hashIndex, h)
	}

	if len(hashIndex) > 0 {
		for _, name := range candidates {
			if claimedFile[name] {
				continue
			}
			hash, hErr := ComputeHash16k(filepath.Join(dir, name))
			if hErr != nil {
				log.Warn("identify: could not hash candidate", "file", name, "err", hErr)
				continue
			}
			ei, ok := hashIndex[hash]
			if !ok {
				continue
			}
			// The first 16 KB agreeing is not enough on its own. The matcher
			// this replaces checked the length too, and dropping that check
			// would let Identify report a truncated file as accounted while
			// relocateFile — which does check — refuses to move it, so the
			// two would disagree about the same file.
			if info, iErr := flatFiles[name].Info(); iErr != nil ||
				(manifest[ei].FileSize > 0 && uint64(info.Size()) != manifest[ei].FileSize) { //nolint:gosec // size is non-negative
				log.Info("identify: Hash16k matched but the length does not",
					"file", name, "par2path", manifest[ei].FileName)
				continue
			}
			delete(hashIndex, hash)
			claimedFile[name] = true
			claimedEntry[ei] = true
			id.Files = append(id.Files, Identified{OnDisk: name, Desc: manifest[ei], By: MatchHash16k})
			log.Info("identify: matched by content",
				"file", name, "par2path", manifest[ei].FileName)
		}
	}

	// Pass 3 — whole-file CRC32, for entries Hash16k could not settle. Gated
	// on the entry carrying IFSC-derived data, since without it there is
	// nothing to compare against.
	// Same ambiguity rule as pass 2, for the same reason: two entries with
	// identical content are indistinguishable, and overwriting the first with
	// the second would silently name a delivered file after whichever entry
	// happened to come last in the manifest.
	crcIndex := make(map[crcSizeKey]int)
	crcAmbiguous := make(map[crcSizeKey]bool)
	for ei, fd := range manifest {
		if claimedEntry[ei] || fd.FileCRC32 == 0 || fd.FileSize == 0 {
			continue
		}
		key := crcSizeKey{crc: fd.FileCRC32, size: fd.FileSize}
		if _, dup := crcIndex[key]; dup {
			crcAmbiguous[key] = true
			continue
		}
		crcIndex[key] = ei
	}
	for k := range crcAmbiguous {
		log.Warn("identify: par2 entries share a CRC32 and length; none can be identified by content",
			"par2path", manifest[crcIndex[k]].FileName)
		delete(crcIndex, k)
	}

	for _, name := range candidates {
		if len(crcIndex) == 0 {
			break
		}
		if claimedFile[name] {
			continue
		}
		de := flatFiles[name]
		info, iErr := de.Info()
		if iErr != nil {
			continue
		}
		size := uint64(info.Size()) //nolint:gosec // size is non-negative
		// Size is the cheap half of the key, so check it before paying for a
		// full read: no entry of this length means no possible match.
		sizeExists := false
		for k := range crcIndex {
			if k.size == size {
				sizeExists = true
				break
			}
		}
		if !sizeExists {
			continue
		}
		crc, cErr := computeFileCRC32(filepath.Join(dir, name))
		if cErr != nil {
			log.Debug("identify: cannot compute CRC32", "file", name, "err", cErr)
			continue
		}
		ei, ok := crcIndex[crcSizeKey{crc: crc, size: size}]
		if !ok {
			continue
		}
		delete(crcIndex, crcSizeKey{crc: crc, size: size})
		claimedFile[name] = true
		claimedEntry[ei] = true
		id.Files = append(id.Files, Identified{OnDisk: name, Desc: manifest[ei], By: MatchCRC32})
		log.Info("identify: matched by whole-file CRC32",
			"file", name, "par2path", manifest[ei].FileName)
	}

	for ei, fd := range manifest {
		if !claimedEntry[ei] {
			id.Unaccounted = append(id.Unaccounted, fd)
			log.Warn("identify: par2 entry matched no delivered file",
				"par2path", fd.FileName, "size", fd.FileSize)
		}
	}

	log.Info("identify: complete",
		"entries", len(manifest),
		"identified", len(id.Files),
		"unaccounted", len(id.Unaccounted),
		"ignored", len(id.Ignored))
	return id, nil
}
