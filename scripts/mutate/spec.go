package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Spec delimiters. They start a line exactly, with no leading whitespace, so
// that a Go anchor — which is nearly always tab-indented — can never be
// mistaken for one.
const (
	delimAnchor  = "--- anchor"
	delimReplace = "--- replace"
	delimEnd     = "--- end"
)

// parseSpec reads a mutation spec file.
//
// The format is line-oriented and delimiter-based rather than YAML or JSON,
// because every anchor in practice is a multi-line, tab-indented run of Go.
// YAML forbids tabs in indentation and JSON would require escaping every one
// of them, so both formats turn the common case into a transcription task —
// and a mistyped anchor is the failure this whole command exists to prevent.
func parseSpec(path string) (*spec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the spec path is this command's argv
	if err != nil {
		return nil, err
	}

	sp := &spec{}
	var cur *mutation
	var section string
	var buf []string
	var sawReplace bool

	// flush attaches the lines collected since the last delimiter.
	flush := func() {
		if cur == nil {
			return
		}
		text := strings.Join(buf, "\n")
		switch section {
		case delimAnchor:
			cur.anchor = text
		case delimReplace:
			cur.replace = text
		}
		buf = nil
	}

	// closeMutation validates and appends the block that just ended.
	closeMutation := func(lineNo int) error {
		if cur == nil {
			return nil
		}
		switch {
		case cur.file == "":
			return fmt.Errorf("line %d: [%s] has no `file` line", lineNo, cur.name)
		case cur.anchor == "":
			return fmt.Errorf("line %d: [%s] has an empty anchor", lineNo, cur.name)
		case !sawReplace:
			// Without this, a missing or misspelled `--- replace` leaves the
			// replacement empty and the mutation DELETES the anchor. That is
			// the shape AGENTS.md warns against — "prefer neutering a
			// condition to deleting a block", because a deletion usually
			// breaks the build and COMPILE_ERROR is not evidence — so the
			// typo would quietly convert a valid pin into a useless run.
			return fmt.Errorf("line %d: [%s] has no `%s` section", lineNo, cur.name, delimReplace)
		case cur.anchor == cur.replace:
			// A no-op mutation always SURVIVES, and reads as though the test
			// failed to discriminate when in truth nothing was changed.
			return fmt.Errorf("line %d: [%s] anchor and replace are identical", lineNo, cur.name)
		}
		sp.mutations = append(sp.mutations, *cur)
		cur = nil
		return nil
	}

	for i, rawLine := range strings.Split(string(data), "\n") {
		lineNo := i + 1

		// Strip the CR once, for content lines as well as delimiters. Doing
		// it only for delimiters left anchors holding CRLF, which then match
		// zero sites in an LF source file — every mutation in a CRLF spec
		// would fail as ANCHOR, blaming the anchor for the file's encoding.
		raw := strings.TrimRight(rawLine, "\r")

		// Inside a content section every line is literal, including blanks
		// and anything that looks like a comment.
		if section != "" {
			switch raw {
			case delimReplace:
				flush()
				section = delimReplace
				sawReplace = true
				continue
			case delimEnd:
				flush()
				section = ""
				if err := closeMutation(lineNo); err != nil {
					return nil, err
				}
				continue
			}
			buf = append(buf, raw)
			continue
		}

		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			if err := closeMutation(lineNo); err != nil {
				return nil, err
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if name == "" {
				return nil, fmt.Errorf("line %d: mutation name is empty", lineNo)
			}
			cur = &mutation{name: name}
			sawReplace = false
			continue
		}

		if line == delimAnchor {
			if cur == nil {
				return nil, fmt.Errorf("line %d: %s outside a [mutation] block", lineNo, delimAnchor)
			}
			section = delimAnchor
			continue
		}

		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("line %d: expected `key value`, got %q", lineNo, line)
		}
		value = strings.TrimSpace(value)

		// pkg, run, tags and timeout configure the whole run. Accepting one
		// inside a [mutation] block would read as per-mutation scoping and
		// silently rewrite the setting for the baseline and for every other
		// mutation, including those already parsed.
		if cur != nil && key != "file" {
			return nil, fmt.Errorf("line %d: `%s` is a global directive and cannot appear inside [%s]", lineNo, key, cur.name)
		}

		switch key {
		case "pkg":
			sp.pkg = value
		case "run":
			sp.run = value
		case "tags":
			sp.tags = value
		case "timeout":
			d, err := time.ParseDuration(value)
			if err != nil {
				return nil, fmt.Errorf("line %d: timeout %q: %w", lineNo, value, err)
			}
			sp.timeout = d
		case "file":
			if cur == nil {
				return nil, fmt.Errorf("line %d: `file` outside a [mutation] block", lineNo)
			}
			cur.file = value
		default:
			return nil, fmt.Errorf("line %d: unknown key %q", lineNo, key)
		}
	}

	if section != "" {
		return nil, fmt.Errorf("unterminated %s section: missing %s", section, delimEnd)
	}
	if cur != nil {
		return nil, fmt.Errorf("[%s] is missing %s", cur.name, delimEnd)
	}
	if sp.pkg == "" {
		return nil, fmt.Errorf("no `pkg` line: nothing to test")
	}
	if len(sp.mutations) == 0 {
		return nil, fmt.Errorf("no [mutation] blocks")
	}
	return sp, nil
}
