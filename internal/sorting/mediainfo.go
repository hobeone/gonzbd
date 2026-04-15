// Package sorting implements media-type detection, sort-string template
// expansion, and directory sorting for completed download jobs.
//
// Library decision: middelink/go-parse-torrent-name was considered but skipped.
// The library hasn't had a release since 2017, and its regex coverage for the
// patterns we need (S01E02, date shows, year-only movies) doesn't add much over
// a handful of inline regexes. Keeping deps minimal also avoids a go.sum entry
// for an unmaintained module.
package sorting

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// MediaType classifies the content of a release name.
type MediaType int

const (
	// UnknownMedia is returned when no known pattern matches.
	UnknownMedia MediaType = iota
	// TVMedia matches releases with a season/episode marker (S01E02).
	TVMedia
	// MovieMedia matches releases whose primary date signal is a standalone year.
	MovieMedia
	// DatedMedia matches daily-show releases with a full date (2024-01-15).
	DatedMedia
)

// MediaInfo holds parsed metadata extracted from a release name. Fields are
// best-effort: zero values mean "not detected."
type MediaInfo struct {
	Type        MediaType
	Title       string
	Year        int    // e.g. 2024
	Season      int    // e.g. 1 for S01E02
	Episode     int    // e.g. 2 for S01E02
	EpisodeName string // e.g. "Pilot" from "Show.S01E01.Pilot.1080p"
	Month       int    // DatedMedia only
	Day         int    // DatedMedia only
	Resolution  string // e.g. "1080p"
}

// Compiled regexes used by Parse.
var (
	// reTV matches SnnEnn patterns (case-insensitive).
	reTV = regexp.MustCompile(`(?i)[Ss](\d+)[Ee](\d+)`)

	// reDate matches YYYY-MM-DD or YYYY.MM.DD.
	reDate = regexp.MustCompile(`\b(\d{4})[-.](\d{2})[-.](\d{2})\b`)

	// reYear matches a standalone 4-digit year in the range 1900–2099.
	reYear = regexp.MustCompile(`\b(19|20)(\d{2})\b`)

	// reRes matches common resolution tags (case-insensitive).
	reRes = regexp.MustCompile(`(?i)\b(480p|720p|1080p|2160p|4k)\b`)

	// reQuality matches common quality/source tags used to delimit the title.
	reQuality = regexp.MustCompile(`(?i)\b(bluray|blu-ray|bdrip|brrip|hdtv|webrip|web-dl|webdl|dvdrip|dvdscr|hdrip|remux|proper|repack)\b`)
)

// Parse extracts MediaInfo from a release name (basename without extension).
// Detection priority: TV > Dated > Movie. Title is extracted from everything
// before the first discriminating pattern.
func Parse(name string) MediaInfo {
	info := MediaInfo{}

	// Resolution — extract early but don't use it to delimit title.
	if m := reRes.FindString(name); m != "" {
		// Normalise: 4K → "2160p" for canonical form, others lower-case.
		switch strings.ToLower(m) {
		case "4k":
			info.Resolution = "2160p"
		default:
			info.Resolution = strings.ToLower(m)
		}
	}

	// TV: S01E02
	if m := reTV.FindStringSubmatchIndex(name); m != nil {
		info.Type = TVMedia
		info.Season, _ = strconv.Atoi(name[m[2]:m[3]])  //nolint:errcheck // regex guarantees digits
		info.Episode, _ = strconv.Atoi(name[m[4]:m[5]]) //nolint:errcheck // regex guarantees digits
		info.Title = cleanTitle(name[:m[0]])

		// Episode name: everything between the SxxExx tag and the first
		// quality/resolution tag (or end), after stripping noise tokens.
		afterSE := name[m[1]:]
		info.EpisodeName = extractEpisodeName(afterSE)

		// Year within the title portion (optional, e.g. "show.2019.S01E02").
		if y := reYear.FindString(name[:m[0]]); y != "" {
			info.Year, _ = strconv.Atoi(y) //nolint:errcheck // regex guarantees digits
		}
		return info
	}

	// Dated: YYYY-MM-DD or YYYY.MM.DD
	if m := reDate.FindStringSubmatchIndex(name); m != nil {
		info.Type = DatedMedia
		info.Year, _ = strconv.Atoi(name[m[2]:m[3]])  //nolint:errcheck // regex guarantees digits
		info.Month, _ = strconv.Atoi(name[m[4]:m[5]]) //nolint:errcheck // regex guarantees digits
		info.Day, _ = strconv.Atoi(name[m[6]:m[7]])   //nolint:errcheck // regex guarantees digits
		info.Title = cleanTitle(name[:m[0]])

		afterDate := name[m[1]:]
		info.EpisodeName = extractEpisodeName(afterDate)
		return info
	}

	// Movie: standalone year (no TV indicator already consumed above).
	if m := reYear.FindStringSubmatchIndex(name); m != nil {
		info.Type = MovieMedia
		info.Year, _ = strconv.Atoi(name[m[0]:m[1]]) //nolint:errcheck // regex guarantees digits
		info.Title = cleanTitle(name[:m[0]])
		return info
	}

	// Fallback: treat the whole name as title, type unknown.
	info.Title = cleanTitle(name)
	return info
}

// cleanTitle converts separator characters to spaces, strips leading/trailing
// whitespace, and collapses runs of spaces. Used to extract a human-readable
// title from a release name fragment.
func cleanTitle(raw string) string {
	// Replace dots and underscores with spaces.
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if r == '.' || r == '_' {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	s := b.String()

	// Collapse runs of whitespace.
	fields := strings.FieldsFunc(s, unicode.IsSpace)
	return strings.Join(fields, " ")
}

// extractEpisodeName tries to pull a human-readable episode name from the
// portion of the release name that follows the TV/date discriminator. It stops
// at common quality/resolution tags or at '(' characters.
func extractEpisodeName(after string) string {
	// Find first quality or resolution boundary.
	end := len(after)
	if m := reQuality.FindStringIndex(after); len(m) == 2 && m[0] < end {
		end = m[0]
	}
	if m := reRes.FindStringIndex(after); len(m) == 2 && m[0] < end {
		end = m[0]
	}
	// Stop at opening parenthesis (e.g. "(720p)").
	if idx := strings.IndexByte(after, '('); idx >= 0 && idx < end {
		end = idx
	}

	candidate := cleanTitle(after[:end])
	// A single-word all-uppercase token is likely a format tag, not a title.
	if candidate == strings.ToUpper(candidate) && !strings.Contains(candidate, " ") {
		return ""
	}
	return candidate
}
