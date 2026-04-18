package nzb

import (
	"regexp"
	"strings"
)

var (
	reSubjectFilenameQuotes = regexp.MustCompile(`"([^"]*)"`)
	// reSubjectBasicFilename ports SABnzbd's basic filename extractor.
	// We use \p{L}\p{N}_ to match any Unicode letter, number or underscore, matching Python 3's \w behavior.
	// We ensure it starts with one of these characters to avoid matching leading symbols or spaces.
	reSubjectBasicFilename = regexp.MustCompile(`([\p{L}\p{N}_][\p{L}\p{N}\-_+()' .,]*(?:\[[\p{L}\p{N}\-_/+()' .,]*][\p{L}\p{N}\-_+()' .,]*)*\.[A-Za-z0-9]{2,4})`)
)

// ExtractFilenameFromSubject attempts to extract a clean filename from a Usenet subject line.
// It follows the logic of Python SABnzbd's subject_name_extractor.
func ExtractFilenameFromSubject(subject string) string {
	// 1. Filename nicely wrapped in quotes
	matches := reSubjectFilenameQuotes.FindAllStringSubmatch(subject, -1)
	for _, m := range matches {
		if len(m) > 1 {
			name := strings.Trim(m[1], ` "`)
			if name != "" {
				return name
			}
		}
	}

	// 2. Found nothing? Try a basic filename-like search
	match := reSubjectBasicFilename.FindString(subject)
	if match != "" {
		return strings.TrimSpace(match)
	}

	// 3. Return the subject
	return subject
}
