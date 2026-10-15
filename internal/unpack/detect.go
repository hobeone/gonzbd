// Package unpack provides functions for detecting, classifying, and extracting
// archive files (RAR, 7-zip, tar, and split-file sets). RAR and 7-zip have
// both a pure-Go extractor (GoUnRAR, GoSevenZip) and one that shells out to
// the external tool (UnRAR, SevenZip); tar (GoTar) and split-file joining
// (FileJoin) are pure Go only.
package unpack

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ArchiveType identifies what kind of archive a path represents.
type ArchiveType int

const (
	// UnknownArchive means the file is not a recognised archive type.
	UnknownArchive ArchiveType = iota
	// RarArchive is a RAR archive or multi-part RAR set.
	RarArchive
	// SevenZipArchive is a 7-zip archive (single .7z or split .7z.001/.7z.002).
	SevenZipArchive
	// SplitArchive represents generic split files (.001/.002/…) with no other
	// recognised archive type; joined by FileJoin.
	SplitArchive
	// TarArchive is a plain .tar archive. Unlike RAR/7z there is no
	// multi-volume split convention in scope here (matches upstream
	// SABnzbd's narrow `TAR_RE = re.compile(r"\.(tar$)", re.I)`), so a
	// TarArchive's Parts is always a single-element slice.
	TarArchive
)

// Archive describes one extractable unit discovered during a directory scan.
type Archive struct {
	// Type is the archive family.
	Type ArchiveType
	// Name is the set name (e.g. "movie", stripped of .part01.rar / .001 suffix).
	Name string
	// MainFile is the file to pass to the unpack tool.
	MainFile string
	// Parts lists all files in the set (for deletion after a successful extract).
	Parts []string
}

// partPattern matches new-style multi-part RAR names: "movie.part01.rar".
var partPattern = regexp.MustCompile(`(?i)\.part\d+\.rar$`)

// legacyExtraPattern matches legacy extra RAR volumes: movie.r00, movie.r01, …
var legacyExtraPattern = regexp.MustCompile(`(?i)\.r\d+$`)

// numericSuffixPattern matches split-file suffixes such as .001, .002.
var numericSuffixPattern = regexp.MustCompile(`\.(\d{3,})$`)

// sevenSplitPattern matches 7z split volumes: .7z.001, .7z.002, …
var sevenSplitPattern = regexp.MustCompile(`(?i)\.7z\.(\d{3,})$`)

// Classify returns the ArchiveType of path based on filename extension.
// Classification is done purely by filename; no magic-byte inspection is
// performed.
func Classify(path string) ArchiveType {
	base := filepath.Base(path)
	lower := strings.ToLower(base)

	switch {
	case partPattern.MatchString(base):
		return RarArchive
	case strings.HasSuffix(lower, ".rar"):
		return RarArchive
	case legacyExtraPattern.MatchString(base):
		return RarArchive
	case sevenSplitPattern.MatchString(base):
		return SevenZipArchive
	case strings.HasSuffix(lower, ".7z"):
		return SevenZipArchive
	case numericSuffixPattern.MatchString(base):
		return SplitArchive
	case strings.HasSuffix(lower, ".tar"):
		return TarArchive
	default:
		return UnknownArchive
	}
}

// Scan returns every Archive found in dir, scanning subdirectories
// recursively. This matches SABnzbd's os.walk()-based archive detection
// in rar_find_archive_sets / sevenzx_find_archive_sets, which is
// essential for NZBs that organize files into subdirectories (e.g.
// release-name/*.rar + Sample/).
func Scan(dir string) ([]Archive, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err // root dir missing/unreadable is fatal
			}
			return nil // skip unreadable child entries
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unpack scan %s: %w", dir, err)
	}

	return groupArchives(files), nil
}

func archiveGroupKey(path, lowerStem string) string {
	return filepath.Join(filepath.Dir(path), lowerStem)
}

// rarSet groups files belonging to one RAR set.
type rarSet struct {
	name  string
	main  string
	parts []string
}

type sevenSplitSet struct {
	name  string
	parts []string
}

type singleArchive struct {
	name string
	path string
}

// groupArchives classifies and groups a flat list of file paths into Archives.
func groupArchives(files []string) []Archive {
	newStyleRar := make(map[string]*rarSet)       // key: dir + lower-case set name
	legacyRar := make(map[string]*rarSet)         // key: dir + lower-case set name
	sevenSplit := make(map[string]*sevenSplitSet) // key: dir + lower-case set name → volume paths
	sevenSingle := make(map[string]singleArchive) // key: dir + lower-case set name → singleArchive
	splitParts := make(map[string][]string)       // key: dir + lower-case base name without suffix
	tarSingle := make(map[string]singleArchive)   // key: dir + lower-case set name → singleArchive

	for _, path := range files {
		base := filepath.Base(path)
		lower := strings.ToLower(base)

		switch {
		case partPattern.MatchString(base):
			// New-style multi-part RAR: movie.part01.rar
			suffix := partPattern.FindString(lower)
			name := lower[:len(lower)-len(suffix)]
			key := archiveGroupKey(path, name)
			s := ensureRarSet(newStyleRar, key)
			s.name = name
			s.parts = append(s.parts, path)

		case strings.HasSuffix(lower, ".rar") && !legacyExtraPattern.MatchString(base):
			// Legacy main RAR: movie.rar
			name := lower[:len(lower)-len(".rar")]
			key := archiveGroupKey(path, name)
			s := ensureRarSet(legacyRar, key)
			s.name = name
			if s.main == "" {
				s.main = path
			}
			s.parts = append(s.parts, path)

		case legacyExtraPattern.MatchString(base):
			// Legacy extra volumes: movie.r00, movie.r01, …
			ext := legacyExtraPattern.FindString(lower)
			name := lower[:len(lower)-len(ext)]
			key := archiveGroupKey(path, name)
			s := ensureRarSet(legacyRar, key)
			s.name = name
			s.parts = append(s.parts, path)

		case sevenSplitPattern.MatchString(base):
			// Split 7z volume: archive.7z.001
			suffix := sevenSplitPattern.FindString(lower)
			name := lower[:len(lower)-len(suffix)]
			key := archiveGroupKey(path, name)
			s, ok := sevenSplit[key]
			if !ok {
				s = &sevenSplitSet{name: name}
				sevenSplit[key] = s
			}
			s.parts = append(s.parts, path)

		case strings.HasSuffix(lower, ".7z"):
			// Single 7z archive.
			name := lower[:len(lower)-len(".7z")]
			key := archiveGroupKey(path, name)
			sevenSingle[key] = singleArchive{name: name, path: path}

		case numericSuffixPattern.MatchString(base):
			// Generic split file: xyz.001
			suffix := numericSuffixPattern.FindString(lower)
			name := lower[:len(lower)-len(suffix)]
			key := archiveGroupKey(path, name)
			splitParts[key] = append(splitParts[key], path)

		case strings.HasSuffix(lower, ".tar"):
			// Plain tar archive. No multi-volume convention in scope.
			name := lower[:len(lower)-len(".tar")]
			key := archiveGroupKey(path, name)
			tarSingle[key] = singleArchive{name: name, path: path}
		}
	}

	var archives []Archive

	// New-style multi-part RARs.
	for _, s := range newStyleRar {
		slices.Sort(s.parts)
		archives = append(archives, Archive{
			Type:     RarArchive,
			Name:     s.name,
			MainFile: s.parts[0],
			Parts:    s.parts,
		})
	}

	// Legacy RARs.
	for _, s := range legacyRar {
		slices.Sort(s.parts)
		main := s.main
		if main == "" && len(s.parts) > 0 {
			main = s.parts[0]
		}
		archives = append(archives, Archive{
			Type:     RarArchive,
			Name:     s.name,
			MainFile: main,
			Parts:    s.parts,
		})
	}

	// Split 7z volumes.
	for _, s := range sevenSplit {
		slices.Sort(s.parts)
		archives = append(archives, Archive{
			Type:     SevenZipArchive,
			Name:     s.name,
			MainFile: s.parts[0],
			Parts:    s.parts,
		})
	}

	// Single 7z archives (skip if covered by a split-volume set of same name in same dir).
	for key, sa := range sevenSingle {
		if _, ok := sevenSplit[key]; ok {
			continue
		}
		archives = append(archives, Archive{
			Type:     SevenZipArchive,
			Name:     sa.name,
			MainFile: sa.path,
			Parts:    []string{sa.path},
		})
	}

	// Generic split files (skip if already covered by a RAR set of the same name in same dir).
	for key, parts := range splitParts {
		if _, ok := newStyleRar[key]; ok {
			continue
		}
		if _, ok := legacyRar[key]; ok {
			continue
		}
		sorted, err := sortedNumericParts(parts)
		if err != nil {
			// Non-contiguous — still emit; FileJoin will return a proper error.
			slices.Sort(parts)
			mainBase := filepath.Base(parts[0])
			suffix := numericSuffixPattern.FindString(mainBase)
			stem := mainBase[:len(mainBase)-len(suffix)]
			archives = append(archives, Archive{
				Type:     SplitArchive,
				Name:     stem,
				MainFile: parts[0],
				Parts:    parts,
			})
			continue
		}
		mainBase := filepath.Base(sorted[0])
		suffix := numericSuffixPattern.FindString(mainBase)
		stem := mainBase[:len(mainBase)-len(suffix)]
		archives = append(archives, Archive{
			Type:     SplitArchive,
			Name:     stem,
			MainFile: sorted[0],
			Parts:    sorted,
		})
	}

	// Single tar archives.
	for _, ta := range tarSingle {
		archives = append(archives, Archive{
			Type:     TarArchive,
			Name:     ta.name,
			MainFile: ta.path,
			Parts:    []string{ta.path},
		})
	}

	// Deterministic sort by Name, then MainFile as tie-breaker for archives
	// with the same Name in different directories.
	slices.SortFunc(archives, func(a, b Archive) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.MainFile, b.MainFile))
	})

	return archives
}

// ensureRarSet returns the existing rarSet for key or creates and inserts one.
func ensureRarSet(m map[string]*rarSet, key string) *rarSet {
	if s, ok := m[key]; ok {
		return s
	}
	s := &rarSet{}
	m[key] = s
	return s
}

// sortedNumericParts sorts parts by their numeric suffix and validates
// contiguity (001, 002, 003, …) starting from 1.
func sortedNumericParts(parts []string) ([]string, error) {
	type numbered struct {
		n    int
		path string
	}

	ns := make([]numbered, 0, len(parts))
	for _, p := range parts {
		base := filepath.Base(p)
		m := numericSuffixPattern.FindStringSubmatch(base)
		if len(m) < 2 {
			return nil, fmt.Errorf("no numeric suffix in %q", base)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, err
		}
		ns = append(ns, numbered{n, p})
	}
	slices.SortFunc(ns, func(a, b numbered) int { return cmp.Compare(a.n, b.n) })

	for i, item := range ns {
		if item.n != i+1 {
			return nil, fmt.Errorf("missing part %d in split sequence", i+1)
		}
	}

	out := make([]string, len(ns))
	for i, item := range ns {
		out[i] = item.path
	}
	return out, nil
}
