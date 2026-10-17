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
	if !strings.EqualFold(subject[:min(len(subject), 9)], "[private]") {
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
//
// A hex-hash name ("ecb7…b2.par2") from the [PRiVATE] or quoted step loses to
// a readable name from either, but beats the unquoted search, which can match
// any dotted token ("Show 1.00"). Any name beats the whole subject: it keeps
// the real extension that par2.IsPar2Name and unpack.Classify test, where a
// sanitized subject ends in "yEnc (1_1)".
func ExtractFilenameFromSubject(subject string) string {
	var hexName string
	// pick returns true when name is a readable candidate to use now, and
	// keeps the first hex candidate as the fallback.
	pick := func(name string) bool {
		if name == "" {
			return false
		}
		if !isExcessivelyObfuscated(name) {
			return true
		}
		if hexName == "" {
			hexName = name
		}
		return false
	}

	// 0. [PRiVATE] tagged subjects
	if name := parsePRiVATESubject(subject); pick(name) {
		return name
	}

	// 1. Filename nicely wrapped in quotes
	matches := reSubjectFilenameQuotes.FindAllStringSubmatch(subject, -1)
	for _, m := range matches {
		if name := strings.Trim(m[1], ` "`); pick(name) {
			return name
		}
	}
	if hexName != "" {
		return hexName
	}

	// 2. Found nothing? Try a basic filename-like search
	if name := strings.TrimSpace(reSubjectBasicFilename.FindString(subject)); name != "" {
		return name
	}

	// 3. Return the subject
	return subject
}
