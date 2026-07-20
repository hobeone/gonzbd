// Package deobfuscate detects and renames obfuscated filenames in completed
// download directories.
//
// Implemented features:
//   - Par2-packet-based renaming (recover original filenames from par2 metadata)
//   - RAR-header-based renaming (extract internal filenames from RAR archives)
//   - IsProbablyObfuscated (full heuristic port from Python)
//   - BiggestFile (3× size ratio guard)
//   - Deobfuscate (rename biggest+siblings to usefulName)
//   - Subtitles (align .srt names to main video)
//   - FixExtension (content-sniff files with missing/wrong extensions)
//   - DVD/Bluray directory skip
package deobfuscate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hobeone/gonzbd/internal/fsutil"
	"github.com/hobeone/gonzbd/internal/rarheader"
)

// excludedExts lists file extensions that are never renamed, matching Python's
// EXCLUDED_FILE_EXTS constant.
var excludedExts = map[string]bool{
	".vob":  true,
	".rar":  true,
	".par2": true,
	".mts":  true,
	".m2ts": true,
	".cpi":  true,
	".clpi": true,
	".mpl":  true,
	".mpls": true,
	".bdm":  true,
	".bdmv": true,
}

// ignoredMovieFolders contains directory names whose presence in a download
// indicates a DVD or Bluray disc structure. Deobfuscation is skipped entirely
// when any of these directories are found, because renaming files inside a
// disc structure would break playback. Matches Python IGNORED_MOVIE_FOLDERS.
var ignoredMovieFolders = map[string]bool{
	"video_ts": true,
	"audio_ts": true,
	"bdmv":     true,
}

// hex32 matches a basename that is exactly 32 lowercase hex digits.
var hex32 = regexp.MustCompile(`^[a-f0-9]{32}$`)

// hex40plus matches a basename that is 40+ chars of lowercase hex + dots only.
var hex40plus = regexp.MustCompile(`^[a-f0-9.]{40,}$`)

// hex30 matches a run of 30+ consecutive lowercase hex digits anywhere in a string.
var hex30 = regexp.MustCompile(`[a-f0-9]{30}`)

// squareBracketWord matches a word wrapped in square brackets.
var squareBracketWord = regexp.MustCompile(`\[\w+\]`)

// abcXyz matches the "abc.xyz" obfuscation prefix.
var abcXyz = regexp.MustCompile(`^abc\.xyz`)

// IsProbablyObfuscated returns true if filename looks obfuscated. The
// argument may be a plain filename or a full path; only the base component
// is inspected. This is a direct port of Python's is_probably_obfuscated.
func IsProbablyObfuscated(log *slog.Logger, filename string) bool {
	// Leaf packages self-scope only when they are the root of the logger chain;
	// a caller-supplied logger is assumed already scoped.
	if log == nil {
		log = slog.Default().With("component", "deobfuscate")
	}
	base := filepath.Base(filename)
	filebasename := strings.TrimSuffix(base, filepath.Ext(base))

	log.Debug("deobfuscate: checking", "basename", filebasename)

	if hasObfuscatedPattern(log, filebasename) {
		return true
	}

	if hasNormalSignals(log, filebasename) {
		return false
	}

	log.Debug("deobfuscate: obfuscated (default)")
	return true
}

func hasObfuscatedPattern(log *slog.Logger, name string) bool {
	// Exactly 32 lowercase hex digits.
	if hex32.MatchString(name) {
		log.Debug("deobfuscate: obfuscated — 32 hex digits")
		return true
	}

	// 40+ chars of lowercase hex + dots.
	if hex40plus.MatchString(name) {
		log.Debug("deobfuscate: obfuscated — 40+ hex/dot chars")
		return true
	}

	// Square-bracket tokens combined with a 30+ hex run.
	if hex30.MatchString(name) && len(squareBracketWord.FindAllString(name, -1)) >= 2 {
		log.Debug("deobfuscate: obfuscated — square brackets + 30-char hex")
		return true
	}

	// Starts with the literal "abc.xyz" prefix.
	if abcXyz.MatchString(name) {
		log.Debug("deobfuscate: obfuscated — abc.xyz prefix")
		return true
	}

	return false
}

//nolint:gocyclo // Sequence of five independent name heuristic checks
func hasNormalSignals(log *slog.Logger, name string) bool {
	// Count character categories. Intentionally ASCII-only to match the
	// Python reference implementation. Non-ASCII runes (unicode letters,
	// CJK, etc.) are not counted toward any bucket, which means filenames
	// consisting entirely of non-ASCII won't match the "not obfuscated"
	// heuristics below and will default to "probably obfuscated" — a
	// conservative but safe behavior.
	decimals := 0
	upperchars := 0
	lowerchars := 0
	spacesdots := 0
	for _, c := range name {
		switch {
		case c >= '0' && c <= '9':
			decimals++
		case c >= 'A' && c <= 'Z':
			upperchars++
		case c >= 'a' && c <= 'z':
			lowerchars++
		case c == ' ' || c == '.' || c == '_':
			spacesdots++
		}
	}

	// "Great Distro" — mixed case with at least one separator.
	if upperchars >= 2 && lowerchars >= 2 && spacesdots >= 1 {
		log.Debug("deobfuscate: not obfuscated — mixed case + separator")
		return true
	}

	// "this is a download" — three or more separators.
	if spacesdots >= 3 {
		log.Debug("deobfuscate: not obfuscated — 3+ separators")
		return true
	}

	// "Beast 2020" — letters + year-like digits + separator.
	if (upperchars+lowerchars >= 4) && decimals >= 4 && spacesdots >= 1 {
		log.Debug("deobfuscate: not obfuscated — letters+digits+sep")
		return true
	}

	// "Catullus" — starts with capital, overwhelmingly lowercase.
	if isCapitalStartMostlyLowercase(name, lowerchars, upperchars) {
		log.Debug("deobfuscate: not obfuscated — capital-start mostly-lowercase")
		return true
	}

	// Short simple words (like "alpha", "multi", "test") are not obfuscated.
	if isShortSimpleWord(name, upperchars, decimals, spacesdots) {
		log.Debug("deobfuscate: not obfuscated — short simple word")
		return true
	}

	return false
}

// BiggestFile returns the largest file in paths (by size on disk). ok is true
// only when paths is non-empty AND the largest file is at least 3× the size
// of the second-largest. When there is exactly one file, it is returned with
// ok=true unconditionally (Python's get_biggest_file returns it as the sole
// candidate).
func BiggestFile(paths []string) (path string, ok bool, err error) {
	if len(paths) == 0 {
		return "", false, nil
	}

	type entry struct {
		path string
		size int64
	}

	entries := make([]entry, 0, len(paths))
	for _, p := range paths {
		fi, statErr := os.Stat(p)
		if statErr != nil {
			// Skip files we can't stat; propagate unexpected errors.
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return "", false, fmt.Errorf("stat %s: %w", p, statErr)
		}
		entries = append(entries, entry{p, fi.Size()})
	}

	if len(entries) == 0 {
		return "", false, nil
	}

	// Find biggest and second-biggest without sorting.
	biggest := entries[0]
	var second entry
	for _, e := range entries[1:] {
		if e.size > biggest.size {
			second = biggest
			biggest = e
		} else if e.size > second.size {
			second = e
		}
	}

	if len(entries) == 1 {
		return biggest.path, true, nil
	}

	// All files are zero bytes — there is no meaningful biggest file.
	if biggest.size == 0 {
		return "", false, nil
	}

	if second.size == 0 {
		// Avoid division by zero; treat as "clearly biggest".
		return biggest.path, true, nil
	}

	if biggest.size >= 3*second.size {
		return biggest.path, true, nil
	}
	return "", false, nil
}

// Rename describes a single file rename performed by Deobfuscate.
type Rename struct {
	From     string
	To       string
	TrueName string // par2-recorded target name; empty for heuristic renames
}

// renameRecorded renames src→dst relative to root and, on success, logs the
// move (logMsg with from/to attributes) and returns the recorded Rename.
// trueName fills Rename.TrueName for par2-driven renames; pass "" for
// heuristic renames.
func renameRecorded(log *slog.Logger, root *os.Root, relSrc, relDst, src, dst, trueName, logMsg string) (Rename, error) {
	if err := root.Rename(relSrc, relDst); err != nil {
		return Rename{}, err
	}
	log.Info(logMsg, "from", src, "to", dst)
	return Rename{From: src, To: dst, TrueName: trueName}, nil
}

// Deobfuscate scans dir for obfuscated files. It first attempts to use PAR2
// metadata for renaming. If no PAR2 files are present or no renames occur,
// it falls back to the "biggest file" heuristic and renames it (and any
// same-stem siblings) to usefulName + original extension. Returns the list
// of renames actually performed. Returns nil, nil when no rename is needed.
//
// Deobfuscation is skipped entirely when the download contains DVD/Bluray
// disc structure directories (VIDEO_TS, AUDIO_TS, BDMV).
func Deobfuscate(ctx context.Context, log *slog.Logger, dir, usefulName string, opts fsutil.SanitizeOptions) ([]Rename, error) {
	if log == nil {
		log = slog.Default().With("component", "deobfuscate")
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("deobfuscate: open root %s: %w", dir, err)
	}
	defer root.Close() //nolint:errcheck // read-only close

	// Skip deobfuscation for DVD/Bluray disc structures.
	if containsIgnoredMovieFolder(root) {
		log.Info("deobfuscate: skipping — DVD/Bluray directory detected", "dir", dir)
		return nil, nil
	}

	// Phase 1: Attempt PAR2-based deobfuscation first.
	parRenames, err := Par2Rename(ctx, log, root, dir, opts)
	if err != nil {
		log.Warn("deobfuscate: par2 deobfuscation encountered an error", "dir", dir, "err", err)
	}
	if len(parRenames) > 0 {
		log.Debug("deobfuscate: par2-based renaming successful — skipping heuristic")
		return parRenames, nil
	}

	// Phase 2: Attempt RAR-header-based deobfuscation.
	rarName := extractRARUsefulName(root, dir, log)
	if rarName != "" {
		log.Info("deobfuscate: RAR headers suggest useful name", "name", rarName)
		usefulName = rarName
	}

	// Phase 3: Collect regular files and fix extensions before the heuristic.
	d, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open root: %w", err)
	}
	defer d.Close() //nolint:errcheck // read-only close
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("readdir: %w", err)
	}

	var paths []string
	var relPaths []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
		relPaths = append(relPaths, e.Name())
	}
	log.Info("deobfuscate: found files for heuristic", "count", len(paths))

	// Fix extensions by content sniffing. Files with non-popular
	// extensions get their real type detected via magic bytes.
	var allRenames []Rename
	for i, rel := range relPaths {
		p := paths[i]
		if !HasPopularExtension(rel) {
			r, fixErr := FixExtension(ctx, log, root, rel, p)
			if fixErr != nil {
				log.Warn("deobfuscate: extension fix error", "path", p, "err", fixErr)
				continue
			}
			if r.From != "" {
				allRenames = append(allRenames, r)
				log.Info("deobfuscate: fixed extension",
					"from", filepath.Base(r.From), "to", filepath.Base(r.To))
				paths[i] = r.To
				relPaths[i] = filepath.Base(r.To)
			}
		}
	}

	bigPath, ok, err := BiggestFile(paths)
	if err != nil {
		return nil, err
	}
	if !ok {
		log.Info("deobfuscate: no qualifying biggest file found (no file 3× larger than next)", "dir", dir)
		return allRenames, nil
	}

	// Phase 4: Heuristic rename of the biggest file.
	// Skip files under 10 MiB — small files (NFOs, scripts, samples)
	// may look obfuscated but renaming them is usually wrong. PAR2 and
	// RAR-header renames above have no size threshold because they use
	// structural metadata, not heuristics.
	const minDeobfuscateSize = 10 * 1024 * 1024 // 10 MiB
	bigRel := filepath.Base(bigPath)
	if fi, statErr := root.Stat(bigRel); statErr == nil && fi.Size() < minDeobfuscateSize {
		log.Info("deobfuscate: biggest file under 10 MiB, skipping heuristic rename",
			"file", bigRel, "size", fi.Size())
		return allRenames, nil
	}

	log.Info("deobfuscate: biggest file candidate", "path", bigRel)

	ext := strings.ToLower(filepath.Ext(bigPath))
	if excludedExts[ext] {
		log.Info("deobfuscate: biggest file has excluded extension, skipping rename",
			"file", bigRel, "ext", ext)
		return allRenames, nil
	}

	if !IsProbablyObfuscated(log, bigPath) {
		log.Info("deobfuscate: biggest file name is not obfuscated, skipping rename",
			"file", bigRel)
		return allRenames, nil
	}
	log.Info("deobfuscate: biggest file is obfuscated, will rename",
		"file", bigRel, "useful_name", usefulName)

	renames := allRenames

	// Rename the biggest file.
	relDst := fsutil.SanitizeFilename(usefulName+filepath.Ext(bigPath), opts)
	newBigRel := fsutil.GetUniqueRelPath(root, relDst)
	newBigPath := filepath.Join(dir, newBigRel)
	r, err := renameRecorded(log, root, bigRel, newBigRel, bigPath, newBigPath, "", "deobfuscate: renamed")
	if err != nil {
		return nil, fmt.Errorf("rename %s → %s: %w", bigPath, newBigPath, err)
	}
	renames = append(renames, r)

	siblingRenames, err := renameSiblings(log, root, dir, usefulName, bigPath, paths, relPaths, allRenames, opts)
	renames = append(renames, siblingRenames...)
	if err != nil {
		return renames, err
	}

	return renames, nil
}

// originalStem returns the path stem (no extension) of bigPath as it was
// BEFORE any extension-fix rename was applied. FixExtension only ever
// appends an extension to the existing name, so if bigPath is the result
// of such a rename, trimming bigPath's own last extension is not enough --
// it leaves any pre-existing pseudo-extension (e.g. ".somejunk" in
// "abc.xyz.somejunk.png") attached to the stem, which breaks matching
// against true siblings that only share the real pre-fix stem.
func originalStem(bigPath string, extensionRenames []Rename) string {
	origPath := bigPath
	for _, r := range extensionRenames {
		if r.To == bigPath {
			origPath = r.From
			break
		}
	}
	return strings.TrimSuffix(origPath, filepath.Ext(origPath))
}

// renameSiblings renames every file in paths/relPaths that shares bigPath's
// pre-fix stem (see originalStem) to usefulName plus its remaining suffix.
// bigPath itself is skipped (the caller already renamed it). Operates
// through root so all writes stay confined to dir, matching the rest of
// this package's os.Root sandboxing.
func renameSiblings(log *slog.Logger, root *os.Root, dir, usefulName, bigPath string, paths, relPaths []string, extensionRenames []Rename, opts fsutil.SanitizeOptions) ([]Rename, error) {
	var renames []Rename
	baseDirFile := originalStem(bigPath, extensionRenames)
	for i, p := range paths {
		if p == bigPath {
			continue
		}
		origP := p
		for _, r := range extensionRenames {
			if r.To == p {
				origP = r.From
				break
			}
		}
		if !strings.HasPrefix(origP, baseDirFile) {
			continue
		}
		rel := relPaths[i]
		if _, err := root.Stat(rel); err != nil {
			continue
		}
		remainingSuffix := strings.TrimPrefix(origP, baseDirFile) + strings.TrimPrefix(p, origP)
		relDstSib := fsutil.SanitizeFilename(usefulName+remainingSuffix, opts)
		newRelSib := fsutil.GetUniqueRelPath(root, relDstSib)
		newPath := filepath.Join(dir, newRelSib)
		r, renErr := renameRecorded(log, root, rel, newRelSib, p, newPath, "", "deobfuscate: renamed sibling")
		if renErr != nil {
			return renames, fmt.Errorf("rename sibling %s → %s: %w", p, newPath, renErr)
		}
		renames = append(renames, r)
	}
	return renames, nil
}

// containsIgnoredMovieFolder walks one level of subdirectories under root and
// returns true if any match an ignored DVD/Bluray folder name.
func containsIgnoredMovieFolder(root *os.Root) bool {
	d, err := root.Open(".")
	if err != nil {
		return false
	}
	defer d.Close() //nolint:errcheck // read-only close
	entries, err := d.ReadDir(-1)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && ignoredMovieFolders[strings.ToLower(e.Name())] {
			return true
		}
	}
	return false
}

// Subtitles renames .srt subtitle files to match the dominant
// video file in dir. This ensures media players auto-detect subtitles.
func Subtitles(log *slog.Logger, dir string) ([]Rename, error) {
	if log == nil {
		log = slog.Default().With("component", "deobfuscate")
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("deobfuscate subtitles: open root %s: %w", dir, err)
	}
	defer root.Close() //nolint:errcheck // read-only close

	d, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("deobfuscate subtitles: open root: %w", err)
	}
	defer d.Close() //nolint:errcheck // read-only close
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("deobfuscate subtitles: readdir: %w", err)
	}

	var paths []string
	var srtRels []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		paths = append(paths, p)
		if strings.EqualFold(filepath.Ext(p), ".srt") {
			srtRels = append(srtRels, e.Name())
		}
	}

	if len(srtRels) == 0 {
		return nil, nil
	}

	biggest, ok, err := BiggestFile(paths)
	if err != nil {
		return nil, err
	}
	if !ok {
		log.Debug("deobfuscate subtitles: no clearly biggest file")
		return nil, nil
	}

	// bigBase is e.g. "/path/to/Some_Big_Movie" (no extension).
	bigBaseRel := strings.TrimSuffix(filepath.Base(biggest), filepath.Ext(biggest))

	var renames []Rename
	for _, srtRel := range srtRels {
		srt := filepath.Join(dir, srtRel)
		srtBaseRel := strings.TrimSuffix(srtRel, filepath.Ext(srtRel))
		if strings.HasPrefix(srtBaseRel, bigBaseRel) {
			suffix := srtBaseRel[len(bigBaseRel):]
			if suffix == "" {
				continue
			}
			nextChar := suffix[0]
			if nextChar == '.' || nextChar == '_' || nextChar == '-' || nextChar == ' ' {
				continue
			}
		}

		// Construct new name: <bigBase>.<srt_filename>
		srtName := filepath.Base(srtRel)
		newRel := fsutil.GetUniqueRelPath(root, bigBaseRel+"."+srtName)
		newPath := filepath.Join(dir, newRel)
		r, renErr := renameRecorded(log, root, srtRel, newRel, srt, newPath, "", "deobfuscate: renamed subtitle")
		if renErr != nil {
			return renames, fmt.Errorf("rename subtitle %s → %s: %w", srt, newPath, renErr)
		}
		renames = append(renames, r)
	}

	return renames, nil
}

// extractRARUsefulName scans root for RAR archives (by magic bytes, not just
// extension) and inspects their headers to extract internal filenames. Returns
// the stem of the most common internal filename, or "" if no useful name can
// be determined.
func extractRARUsefulName(root *os.Root, dir string, log *slog.Logger) string {
	if log == nil {
		log = slog.Default().With("component", "deobfuscate")
	}
	d, err := root.Open(".")
	if err != nil {
		return ""
	}
	defer d.Close() //nolint:errcheck // read-only close
	entries, err := d.ReadDir(-1)
	if err != nil {
		return ""
	}

	// Collect internal filenames from all RAR files in the directory.
	var internalNames []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())

		f, err := root.Open(e.Name())
		if err != nil {
			continue
		}
		isRAR, rerr := rarheader.IsRARReader(f)
		_ = f.Close()
		if rerr != nil || !isRAR {
			continue
		}

		info, err := rarheader.Inspect(path)
		if err != nil {
			log.Debug("deobfuscate: RAR inspect failed", "path", path, "err", err)
			continue
		}

		if info.HeaderEncrypted {
			log.Info("deobfuscate: RAR has encrypted headers — cannot extract names", "path", path)
			continue
		}

		internalNames = append(internalNames, info.Filenames...)
	}

	if len(internalNames) == 0 {
		return ""
	}

	// Find the most common filename stem across all archives.
	// This handles split volumes where each part contains the same file list.
	stemCounts := make(map[string]int)
	for _, name := range internalNames {
		// RAR internal paths use '/' as separator. Take the basename.
		base := filepath.Base(name)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		if stem != "" && !IsProbablyObfuscated(log, stem) {
			stemCounts[stem]++
		}
	}

	if len(stemCounts) == 0 {
		return ""
	}

	var bestStem string
	var bestCount int
	for stem, count := range stemCounts {
		if count > bestCount {
			bestStem = stem
			bestCount = count
		}
	}
	return bestStem
}

func isCapitalStartMostlyLowercase(basename string, lower, upper int) bool {
	if basename == "" {
		return false
	}
	first := basename[0]
	return first >= 'A' && first <= 'Z' && lower > 2 && upper > 0 && float64(upper)/float64(lower) <= 0.25
}

func isShortSimpleWord(basename string, upper, decs, seps int) bool {
	return len(basename) >= 3 && len(basename) <= 10 && upper == 0 && decs == 0 && seps <= 1
}
