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
//     reason is mandatory; a bare marker is itself reported. The marker must
//     start the comment line — a sentence that merely names it is prose, not a
//     marker — and a reason that wraps must be closed by a blank // line
//     unless the marker is the block's last line. An unclosed wrapped reason
//     is a hard error rather than a guess: see splitMarker for why the guess
//     silently suppresses the finding on the OTHER copy.
package main

import (
	"bufio"
	"errors"
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
//
// ANCHORED to the start of the comment line, because an unanchored match
// makes any prose that merely NAMES the marker into one. Every sentence of the
// form "add a //dupcomment:ok <reason> line" — including several in this file
// and one in internal/postproc — was exempting the block it sat in, and the
// captured "reason" was the rest of the sentence. Scanned lines have their //
// stripped and are trimmed, so ^ is exactly "first thing on the line", which
// is where every real marker in the tree already sits.
var markerRe = regexp.MustCompile(`^dupcomment:ok\s*(.*)$`)

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
	var markerErrs []error
	for _, f := range files {
		blocks, mErrs, err := scan(f, *minLines, *minChars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "check_dup_comments: %s: %v\n", f, err)
			os.Exit(2)
		}
		markerErrs = append(markerErrs, mErrs...)
		for key, occs := range blocks {
			groups[key] = append(groups[key], occs...)
		}
	}
	// Before the duplicate report, and fatal on its own: an unterminated
	// marker makes its block hash differently from its twin, so continuing
	// would print a duplicate report that is missing exactly the groups this
	// error is about.
	if len(markerErrs) > 0 {
		for _, e := range markerErrs {
			fmt.Fprintf(os.Stderr, "check_dup_comments: %v\n", e)
		}
		os.Exit(2)
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
func scan(path string, minLines, minChars int) (blocks map[string][]occurrence, markerErrs []error, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: paths come from git ls-files or argv
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	blocks = map[string][]occurrence{}
	var cur []string
	start := 0
	lineNo := 0

	flush := func() {
		defer func() { cur = nil }()
		body, reason, mErr := splitMarker(cur)
		if mErr != nil {
			// Recorded rather than returned immediately so one run names
			// every offending block instead of only the first.
			markerErrs = append(markerErrs, fmt.Errorf("%s:%d: %w", path, start, mErr))
			return
		}
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
	return blocks, markerErrs, sc.Err()
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

// errUnterminatedMarker reports a //dupcomment:ok whose paragraph is not
// closed by a blank comment line while further comment lines follow it.
var errUnterminatedMarker = errors.New("check_dup_comments: unterminated //dupcomment:ok reason")

// splitMarker separates a raw comment block into the lines that form its
// identity and the //dupcomment:ok reason, if any.
//
// The marker owns a PARAGRAPH — its own line plus every following line up to a
// blank comment line — because a one-line reason is the exception.
//
// An unterminated marker with lines after it is REFUSED rather than guessed
// at, because the guess is unrecoverable in both directions. Consuming to the
// end of the block erases the very text being identified; consuming only the
// marker's line leaves a wrapped reason in the key. Either way the marked
// copy hashes differently from its unmarked twin, the group falls below two
// occurrences, and NEITHER is reported — so adding a marker to one copy
// silences the other, which is the exact inversion of what the marker means.
//
// That was not hypothetical. It reproduced against this tool with a two-line
// reason and no closing `//`: the control run reported the pair, and the run
// with the marker exited 0.
//
// Undecidable, hence an error rather than a rule: nothing in the text says
// whether the line after the marker continues the reason or resumes the
// comment. The single-line-reason case is still fine unterminated when the
// marker is the block's LAST line, which is where it usually sits, and a BARE
// marker is never refused because it has no reason that could wrap.
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
func splitMarker(raw []string) (body []string, reason string, err error) {
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
		if i == len(raw)-1 || reason == "" || strings.TrimSpace(m[1]) == "" {
			// Nothing to be ambiguous about. Either the marker is the block's
			// last line so nothing can follow it, or it is BARE — and a bare
			// marker has no reason to continue, so whatever comes next is
			// block text by definition. That keeps the narrowest useful
			// error: only a marker that carries a reason can have a wrapped
			// one, and only then is the following line undecidable.
			continue
		}
		end := -1
		for j := i + 1; j < len(raw); j++ {
			if raw[j] == "" {
				end = j
				break
			}
		}
		if end < 0 {
			return nil, "", fmt.Errorf("%w: close it with a blank `//` line, or move it to the end of the block", errUnterminatedMarker)
		}
		for k := i + 1; k < end; k++ {
			skip[k] = true
		}
	}
	for i, l := range raw {
		if skip[i] || l == "" {
			continue
		}
		body = append(body, wsRun.ReplaceAllString(l, " "))
	}
	return body, reason, nil
}
