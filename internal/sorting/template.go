package sorting

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// tokenEntry is one expand-able template token.
type tokenEntry struct {
	token string
	value func(info MediaInfo, ext string) string
}

// tokens lists every supported template token in longest-match order so that
// e.g. %ext is tested before %e, and %0s before %s. The order of entries
// determines which token wins when one is a prefix of another — first match
// at any given % position wins.
var tokens = []tokenEntry{
	// Extension — must come before %e (Python's special-case comment).
	{"%ext", func(info MediaInfo, ext string) string { return strings.TrimPrefix(ext, ".") }},
	// Episode name variants — must come before bare %e.
	{"%e.n", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.EpisodeName, " ", ".") }},
	{"%e_n", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.EpisodeName, " ", "_") }},
	{"%en", func(info MediaInfo, _ string) string { return info.EpisodeName }},
	// Episode number — after episode-name tokens.
	{"%0e", func(info MediaInfo, _ string) string { return fmt.Sprintf("%02d", info.Episode) }},
	{"%e", func(info MediaInfo, _ string) string { return fmt.Sprintf("%d", info.Episode) }},
	// Title long-form aliases (SABnzbd %title, %sN, %s.N, %s_N — all map to info.Title).
	{"%title", func(info MediaInfo, _ string) string { return info.Title }},
	{"%.title", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", ".") }},
	{"%_title", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", "_") }},
	{"%s.N", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", ".") }},
	{"%s_N", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", "_") }},
	{"%sN", func(info MediaInfo, _ string) string { return info.Title }},
	// Title short-form (existing).
	{"%_.t", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", "_") }},
	{"%.t", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", ".") }},
	{"%_t", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", "_") }},
	{"%t", func(info MediaInfo, _ string) string { return info.Title }},
	// Original job name (SABnzbd %dn).
	{"%dn", func(info MediaInfo, _ string) string { return info.OriginalJobName }},
	// Legacy episode name alias (SABnzbd %desc — maps to episode name for date shows, empty otherwise).
	{"%desc", func(info MediaInfo, _ string) string {
		if info.Type == DatedMedia {
			return info.EpisodeName
		}
		return ""
	}},
	// Year — long-form alias (SABnzbd %year).
	{"%year", func(info MediaInfo, _ string) string {
		if info.Year == 0 {
			return ""
		}
		return fmt.Sprintf("%d", info.Year)
	}},
	// Decade — zero-padded 4-digit (SABnzbd %0decade, e.g. "2020").
	{"%0decade", func(info MediaInfo, _ string) string {
		if info.Year == 0 {
			return ""
		}
		return fmt.Sprintf("%d", (info.Year/10)*10)
	}},
	// Decade — short form with 's' suffix (e.g. "2020s").
	{"%decade", func(info MediaInfo, _ string) string {
		if info.Year == 0 {
			return ""
		}
		return fmt.Sprintf("%ds", (info.Year/10)*10)
	}},
	{"%y", func(info MediaInfo, _ string) string {
		if info.Year == 0 {
			return ""
		}
		return fmt.Sprintf("%d", info.Year)
	}},
	// Season.
	{"%0s", func(info MediaInfo, _ string) string { return fmt.Sprintf("%02d", info.Season) }},
	{"%s", func(info MediaInfo, _ string) string { return fmt.Sprintf("%d", info.Season) }},
	// Month.
	{"%0m", func(info MediaInfo, _ string) string { return fmt.Sprintf("%02d", info.Month) }},
	{"%m", func(info MediaInfo, _ string) string { return fmt.Sprintf("%d", info.Month) }},
	// Day.
	{"%0d", func(info MediaInfo, _ string) string { return fmt.Sprintf("%02d", info.Day) }},
	{"%d", func(info MediaInfo, _ string) string { return fmt.Sprintf("%d", info.Day) }},
	// Resolution.
	{"%r", func(info MediaInfo, _ string) string { return info.Resolution }},
}

// reLowercaseBraces matches {content} wrappers for lowercasing.
var reLowercaseBraces = regexp.MustCompile(`\{([^}]*)\}`)

// ExpandTemplate substitutes sort-string tokens in template with values
// derived from info. Unknown tokens beginning with % are left verbatim.
// The special %ext token is replaced with ext (which should include a leading
// dot, e.g. ".mkv"); if ext is empty, %ext expands to an empty string.
//
// Longest-match-first scanning at each % ensures that %ext is matched before
// %e, and %0s before %s, matching Python's path_subst behaviour.
func ExpandTemplate(template string, info MediaInfo, ext string) string {
	var out strings.Builder
	out.Grow(len(template))

	n := 0
	for n < len(template) {
		if template[n] != '%' {
			out.WriteByte(template[n])
			n++
			continue
		}

		// Try every token starting at position n (tokens already sorted
		// longest-prefix-first within logically related groups by construction).
		matched := false
		for _, tok := range tokens {
			if strings.HasPrefix(template[n:], tok.token) {
				out.WriteString(tok.value(info, ext))
				n += len(tok.token)
				matched = true
				break
			}
		}
		if !matched {
			// Unknown token — emit the literal '%' and advance one byte.
			out.WriteByte('%')
			n++
		}
	}

	result := out.String()

	// SABnzbd {…} lowercase wrapper: lowercase everything inside braces.
	result = reLowercaseBraces.ReplaceAllStringFunc(result, func(m string) string {
		// Strip the braces and lowercase the content.
		return strings.ToLower(m[1 : len(m)-1])
	})

	return result
}

// CleanTemplatePath cleans up a template-expanded path by stripping leading
// and trailing whitespace, underscores, and hyphens from each path element,
// then collapsing runs of repeated dots or underscores. This prevents messy
// directory names like "Show Name -  /Season 01" when template tokens
// expand to empty strings. Matches SABnzbd's strip_path_elements.
//
// Empty elements (which would create leading or double slashes) are removed.
func CleanTemplatePath(path string) string {
	// Work with forward slashes internally.
	path = filepath.ToSlash(path)
	parts := strings.Split(path, "/")
	var clean []string
	for _, p := range parts {
		p = stripElement(p)
		if p != "" {
			clean = append(clean, p)
		}
	}
	return strings.Join(clean, "/")
}

// stripElement strips one path element of leading/trailing whitespace,
// underscores, hyphens, and dots. It also collapses repeated dots (..)
// and underscores (__) into single characters.
func stripElement(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool {
		return r == ' ' || r == '_' || r == '-'
	})
	// Collapse repeated dots and underscores.
	s = collapseRun(s, '.')
	s = collapseRun(s, '_')
	// Strip trailing dots (problematic on Windows and ugly everywhere).
	s = strings.TrimRight(s, ".")
	return s
}

// collapseRun replaces runs of two or more occurrences of c with a single c.
func collapseRun(s string, c byte) string {
	var b strings.Builder
	b.Grow(len(s))
	prev := byte(0)
	for i := range len(s) {
		ch := s[i]
		if ch == c && prev == c {
			continue
		}
		b.WriteByte(ch)
		prev = ch
	}
	return b.String()
}
