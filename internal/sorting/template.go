package sorting

import (
	"fmt"
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
	// Title variants.
	{"%_.t", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", "_") }},
	{"%.t", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", ".") }},
	{"%_t", func(info MediaInfo, _ string) string { return strings.ReplaceAll(info.Title, " ", "_") }},
	{"%t", func(info MediaInfo, _ string) string { return info.Title }},
	// Year and decade.
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

	return out.String()
}
