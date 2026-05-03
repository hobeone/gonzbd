package nzb

import (
	"regexp"
	"strings"
)

var (
	reSubjectFilenameQuotes = regexp.MustCompile(`"([^"]*)"`)
	// reSubjectBasicFilename ports SABnzbd's basic filename extractor.
	// We use \p{L}\p{N}_ to match any Unicode letter, number or underscore,
	// matching Python 3's \w behavior.
	// We ensure it starts with one of these characters to avoid matching leading symbols or spaces.
	reSubjectBasicFilename = regexp.MustCompile(`([\p{L}\p{N}_][\p{L}\p{N}\-_+()' .,]*(?:\[[\p{L}\p{N}\-_/+()' .,]*][\p{L}\p{N}\-_+()' .,]*)*\.[A-Za-z0-9]{2,4})`)

	reBracketGroups        = regexp.MustCompile(`\[([^\]]+)\]`)
	rePureHex              = regexp.MustCompile(`(?i)^[0-9a-f]{16,}$`)
	reExcessiveObfuscation = regexp.MustCompile(`(?i)^[0-9a-f]{16,}\.[a-z0-9]{2,4}$`)
)

func isExcessivelyObfuscated(name string) bool {
	return reExcessiveObfuscation.MatchString(name)
}

func parsePRiVATESubject(subject string) string {
	if len(subject) < 9 || !strings.EqualFold(subject[:9], "[private]") {
		return ""
	}

	matches := reBracketGroups.FindAllStringSubmatch(subject, -1)
	if len(matches) >= 3 {
		name := matches[2][1]
		if rePureHex.MatchString(name) {
			return ""
		}
		return name
	}
	return ""
}

// ExtractFilenameFromSubject attempts to extract a clean filename from a Usenet subject line.
// It follows the logic of Python SABnzbd's subject_name_extractor.
func ExtractFilenameFromSubject(subject string) string {
	// 0. [PRiVATE] tagged subjects
	if name := parsePRiVATESubject(subject); name != "" && !isExcessivelyObfuscated(name) {
		return name
	}

	// 1. Filename nicely wrapped in quotes
	matches := reSubjectFilenameQuotes.FindAllStringSubmatch(subject, -1)
	for _, m := range matches {
		if len(m) > 1 {
			name := strings.Trim(m[1], ` "`)
			if name != "" && !isExcessivelyObfuscated(name) {
				return name
			}
		}
	}

	// 2. Found nothing? Try a basic filename-like search
	match := reSubjectBasicFilename.FindString(subject)
	if match != "" {
		name := strings.TrimSpace(match)
		if !isExcessivelyObfuscated(name) {
			return name
		}
	}

	// 3. Return the subject
	return subject
}
