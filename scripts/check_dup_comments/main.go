// Command check_dup_comments reports multi-line // comment blocks that appear
// more than once across the repository's Go sources.
//
// It exists because a duplicated comment block is invisible to every other
// gate. go build, go vet, golangci-lint and the whole test suite are
// structurally blind to it: the text is a comment, so nothing type-checks it
// and nothing executes it. One shipped on the download-durability branch, and
// the AGENTS.md claim sweep does not catch the class either — that sweep greps
// for claims it knows are WRONG, and a duplicated block is usually a claim that
// is right in one place and wrong in the other.
//
// The two failure shapes it is aimed at:
//
//   - A package doc comment pasted into a second file of the same package, so
//     godoc picks one of them arbitrarily.
//   - A block copied to a new declaration and left naming the ORIGINAL
//     declaration, so the copy documents a function it does not sit on. This is
//     the expensive one: the comment reads as authoritative and describes
//     different code.
//
// Two exemptions, both deliberate:
//
//   - Occurrences whose files all share one basename are allowed. That is the
//     shape of a per-package test helper (ackhelpers_test.go in five packages),
//     which Go cannot share across package boundaries, so the copies are
//     required rather than accidental.
//   - A block containing a //dupcomment:ok <reason> line is allowed, following
//     the //lockio: and //nocover: convention already used in this repo. The
//     reason is mandatory; a bare marker is itself reported.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// occurrence is one appearance of a normalised comment block.
type occurrence struct {
	file   string
	line   int    // 1-based line of the block's first // line
	n      int    // how many // lines the block spans
	reason string // the //dupcomment:ok reason on THIS occurrence, if any
}

// The defaults. min-lines was 4 until a sweep showed that the documented
// failure shape — a block copied across packages and left naming the original
// declaration — routinely fits in three lines, so 4 made the tool's own stated
// purpose unreportable. min-chars carries most of the filtering work; three
// lines totalling 120+ normalised characters is prose, not boilerplate.
const (
	defaultMinLines = 3
	defaultMinChars = 120
)

var wsRun = regexp.MustCompile(`\s+`)

// markerRe matches the exemption marker and optionally captures its reason.
//
// The reason is required to EXEMPT, but the marker is stripped from the
// normalised text whether or not it carries one — and those must be two
// separate decisions. An earlier version matched only `dupcomment:ok\s+(\S.*)`,
// so a bare marker was not recognised at all, stayed in the normalised text,
// and made the block hash differently from its twin. The group then failed to
// match and nothing was reported: the bare marker suppressed the finding by
// accident, which is the opposite of the intent and is invisible from the
// outside. TestScan_BareMarkerDoesNotExempt caught it and pins it.
var markerRe = regexp.MustCompile(`dupcomment:ok\s*(.*)$`)

func main() {
	minLines := flag.Int("min-lines", defaultMinLines, "minimum consecutive // lines for a block to be considered")
	minChars := flag.Int("min-chars", defaultMinChars, "minimum normalised length for a block to be considered")
	flag.Parse()

	files, err := goFiles(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "check_dup_comments: %v\n", err)
		os.Exit(2)
	}

	groups := map[string][]occurrence{}
	for _, f := range files {
		blocks, err := scan(f, *minLines, *minChars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "check_dup_comments: %s: %v\n", f, err)
			os.Exit(2)
		}
		for key, occs := range blocks {
			groups[key] = append(groups[key], occs...)
		}
	}

	var keys []string
	for key, occs := range groups {
		if len(occs) < 2 || sameBasename(occs) || allMarked(occs) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })

	if len(keys) == 0 {
		return
	}
	for _, key := range keys {
		occs := groups[key]
		sort.Slice(occs, func(i, j int) bool {
			if occs[i].file != occs[j].file {
				return occs[i].file < occs[j].file
			}
			return occs[i].line < occs[j].line
		})
		fmt.Printf("duplicated comment block (%d lines) at %d places:\n", occs[0].n, len(occs))
		for _, o := range occs {
			fmt.Printf("  %s:%d\n", o.file, o.line)
		}
		for l := range strings.SplitSeq(key, "\n") {
			fmt.Printf("    | %s\n", l)
		}
		fmt.Println()
	}
	fmt.Fprintf(os.Stderr, "check_dup_comments: %d duplicated comment block(s).\n"+
		"Rewrite the copy so it describes the declaration it sits on, or add a\n"+
		"//dupcomment:ok <reason> line inside the block if the repetition is required.\n", len(keys))
	os.Exit(1)
}

// allMarked reports whether EVERY occurrence in the group carries an exemption
// marker with a reason.
//
// Every, not any. Keying the exemption on the block's text alone — which is
// what the first version of this file did, via a repo-wide map from normalised
// text to reason — meant a single //dupcomment:ok anywhere permanently
// whitelisted that text everywhere, including in a copy pasted by accident
// months later. The marker would then be doing the opposite of its job: the
// deliberate repetition it was written for would keep the accidental one
// invisible. Requiring it on each occurrence costs one comment line per copy
// and makes a new copy re-trigger the report, which is the behaviour the gate
// exists for.
func allMarked(occs []occurrence) bool {
	for _, o := range occs {
		if o.reason == "" {
			return false
		}
	}
	return true
}

// sameBasename reports whether every occurrence lives in a file with the same
// base name, which is the per-package-test-helper shape described in the
// command doc. A group with two occurrences in ONE file is not exempt: that is
// a copy-paste within a single file, the shape this tool most wants to catch,
// so the distinct-directory count has to be the number of occurrences.
func sameBasename(occs []occurrence) bool {
	base := filepath.Base(occs[0].file)
	dirs := map[string]bool{}
	for _, o := range occs {
		if filepath.Base(o.file) != base {
			return false
		}
		dirs[filepath.Dir(o.file)] = true
	}
	return len(dirs) == len(occs)
}

// scan extracts every qualifying comment block from one file, keyed by
// normalised text. Each occurrence carries the exemption reason found on that
// occurrence, if any; allMarked decides what to do with them.
func scan(path string, minLines, minChars int) (blocks map[string][]occurrence, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: paths come from git ls-files or argv
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	blocks = map[string][]occurrence{}
	var cur []string
	start := 0
	lineNo := 0

	flush := func() {
		defer func() { cur = nil }()
		body, reason := splitMarker(cur)
		if len(body) < minLines {
			return
		}
		key := strings.Join(body, "\n")
		if len(key) < minChars {
			return
		}
		blocks[key] = append(blocks[key], occurrence{file: path, line: start, n: len(body), reason: reason})
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		lineNo++
		text := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(text, "//") {
			flush()
			continue
		}
		if len(cur) == 0 {
			start = lineNo
		}
		cur = append(cur, strings.TrimSpace(strings.TrimPrefix(text, "//")))
	}
	flush()
	return blocks, sc.Err()
}

// goFiles resolves the files to scan: the arguments if any were given,
// otherwise every tracked .go file. git ls-files rather than a filepath.Walk
// so ui/dist, node_modules and build output stay out without a skip list.
func goFiles(args []string) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}
	out, err := exec.Command("git", "ls-files", "*.go").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// splitMarker separates a raw comment block into the lines that form its
// identity and the //dupcomment:ok reason, if any.
//
// The marker owns a PARAGRAPH — its own line plus every following line up to a
// blank comment line — because a one-line reason is the exception. If no blank
// line terminates it, only the marker's own line is consumed; swallowing to the
// end of the block instead would erase the very text being identified, which is
// how an unterminated bare marker made its whole block disappear from the key.
//
// This runs over the collected block rather than while scanning, so the
// "is it terminated?" question is answerable at all. Doing it in one streaming
// pass is what produced three separate suppression-by-accident bugs in this
// file: an unmatched bare marker, a blank line after a matched one, and a
// multi-line reason. The rule they all violate is the same — marker placement
// and length must not affect the key — and only a two-pass split enforces it
// structurally rather than case by case.
//
// Empty lines are dropped from the identity for the same reason: they are
// paragraph breaks, and a marker is conventionally followed by one.
func splitMarker(raw []string) (body []string, reason string) {
	skip := map[int]bool{}
	for i, l := range raw {
		m := markerRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if r := strings.TrimSpace(m[1]); r != "" && reason == "" {
			reason = r
		}
		skip[i] = true
		// Consume the continuation only when a blank line closes it.
		for j := i + 1; j < len(raw); j++ {
			if raw[j] == "" {
				for k := i + 1; k < j; k++ {
					skip[k] = true
				}
				break
			}
		}
	}
	for i, l := range raw {
		if skip[i] || l == "" {
			continue
		}
		body = append(body, wsRun.ReplaceAllString(l, " "))
	}
	return body, reason
}
