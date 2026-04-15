// Package unpack provides functions for detecting, classifying, and extracting
// archive files (RAR, 7-zip, and split-file sets) by shelling out to the
// appropriate command-line tools.
package unpack

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
var sevenSplitPattern = regexp.MustCompile(`(?i)\.7z\.(\d+)$`)

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
	default:
		return UnknownArchive
	}
}

// Scan returns every Archive found directly in dir (non-recursive).
// Files that are not recognised archive types are ignored.
func Scan(dir string) ([]Archive, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("unpack scan %s: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}

	return groupArchives(files), nil
}

// rarSet groups files belonging to one RAR set.
type rarSet struct {
	main  string
	parts []string
}

// groupArchives classifies and groups a flat list of file paths into Archives.
func groupArchives(files []string) []Archive {
	newStyleRar := make(map[string]*rarSet) // key: lower-case set name
	legacyRar := make(map[string]*rarSet)   // key: lower-case set name
	sevenSplit := make(map[string][]string) // key: lower-case set name → volume paths
	sevenSingle := make(map[string]string)  // key: lower-case set name → .7z path
	splitParts := make(map[string][]string) // key: lower-case base name without suffix

	for _, path := range files {
		base := filepath.Base(path)
		lower := strings.ToLower(base)

		switch {
		case partPattern.MatchString(base):
			// New-style multi-part RAR: movie.part01.rar
			suffix := partPattern.FindString(lower)
			name := lower[:len(lower)-len(suffix)]
			s := ensureRarSet(newStyleRar, name)
			s.parts = append(s.parts, path)

		case strings.HasSuffix(lower, ".rar") && !legacyExtraPattern.MatchString(base):
			// Legacy main RAR: movie.rar
			name := lower[:len(lower)-len(".rar")]
			s := ensureRarSet(legacyRar, name)
			if s.main == "" {
				s.main = path
			}
			s.parts = append(s.parts, path)

		case legacyExtraPattern.MatchString(base):
			// Legacy extra volumes: movie.r00, movie.r01, …
			ext := legacyExtraPattern.FindString(lower)
			name := lower[:len(lower)-len(ext)]
			s := ensureRarSet(legacyRar, name)
			s.parts = append(s.parts, path)

		case sevenSplitPattern.MatchString(base):
			// Split 7z volume: archive.7z.001
			suffix := sevenSplitPattern.FindString(lower)
			name := lower[:len(lower)-len(suffix)]
			sevenSplit[name] = append(sevenSplit[name], path)

		case strings.HasSuffix(lower, ".7z"):
			// Single 7z archive.
			name := lower[:len(lower)-len(".7z")]
			sevenSingle[name] = path

		case numericSuffixPattern.MatchString(base):
			// Generic split file: xyz.001
			suffix := numericSuffixPattern.FindString(lower)
			name := lower[:len(lower)-len(suffix)]
			splitParts[name] = append(splitParts[name], path)
		}
	}

	var archives []Archive

	// New-style multi-part RARs.
	for name, s := range newStyleRar {
		sort.Strings(s.parts)
		archives = append(archives, Archive{
			Type:     RarArchive,
			Name:     name,
			MainFile: s.parts[0],
			Parts:    s.parts,
		})
	}

	// Legacy RARs.
	for name, s := range legacyRar {
		sort.Strings(s.parts)
		main := s.main
		if main == "" && len(s.parts) > 0 {
			main = s.parts[0]
		}
		archives = append(archives, Archive{
			Type:     RarArchive,
			Name:     name,
			MainFile: main,
			Parts:    s.parts,
		})
	}

	// Split 7z volumes.
	for name, parts := range sevenSplit {
		sort.Strings(parts)
		archives = append(archives, Archive{
			Type:     SevenZipArchive,
			Name:     name,
			MainFile: parts[0],
			Parts:    parts,
		})
	}

	// Single 7z archives (skip if covered by a split-volume set of same name).
	for name, path := range sevenSingle {
		if _, ok := sevenSplit[name]; ok {
			continue
		}
		archives = append(archives, Archive{
			Type:     SevenZipArchive,
			Name:     name,
			MainFile: path,
			Parts:    []string{path},
		})
	}

	// Generic split files (skip if already covered by a RAR set of the same name).
	for name, parts := range splitParts {
		if _, ok := newStyleRar[name]; ok {
			continue
		}
		if _, ok := legacyRar[name]; ok {
			continue
		}
		sorted, err := sortedNumericParts(parts)
		if err != nil {
			// Non-contiguous — still emit; FileJoin will return a proper error.
			sort.Strings(parts)
			archives = append(archives, Archive{
				Type:     SplitArchive,
				Name:     name,
				MainFile: parts[0],
				Parts:    parts,
			})
			continue
		}
		archives = append(archives, Archive{
			Type:     SplitArchive,
			Name:     name,
			MainFile: sorted[0],
			Parts:    sorted,
		})
	}

	// Stable sort by Name for deterministic output.
	sort.Slice(archives, func(i, j int) bool { return archives[i].Name < archives[j].Name })

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
	sort.Slice(ns, func(i, j int) bool { return ns[i].n < ns[j].n })

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
