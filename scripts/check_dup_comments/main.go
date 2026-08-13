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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// occurrence is one appearance of a normalised comment block.
type occurrence struct {
	file string
	line int // 1-based line of the block's first // line
	n    int // how many // lines the block spans
}

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
	minLines := flag.Int("min-lines", 4, "minimum consecutive // lines for a block to be considered")
	minChars := flag.Int("min-chars", 120, "minimum normalised length for a block to be considered")
	flag.Parse()

	files, err := goFiles(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "check_dup_comments: %v\n", err)
		os.Exit(2)
	}

	groups := map[string][]occurrence{}
	marked := map[string]string{}
	for _, f := range files {
		blocks, marks, err := scan(f, *minLines, *minChars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "check_dup_comments: %s: %v\n", f, err)
			os.Exit(2)
		}
		for key, occs := range blocks {
			groups[key] = append(groups[key], occs...)
		}
		maps.Copy(marked, marks)
	}

	var keys []string
	for key, occs := range groups {
		if len(occs) < 2 || sameBasename(occs) || marked[key] != "" {
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

// scan extracts every qualifying comment block from one file. It returns the
// blocks keyed by normalised text, and separately the keys carrying an
// exemption marker, so a marker on any one copy exempts the group.
func scan(path string, minLines, minChars int) (blocks map[string][]occurrence, marked map[string]string, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: paths come from git ls-files or argv
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	blocks, marked = map[string][]occurrence{}, map[string]string{}
	var cur []string
	var reason string
	start := 0
	lineNo := 0

	flush := func() {
		defer func() { cur, reason = nil, "" }()
		if len(cur) < minLines {
			return
		}
		key := strings.Join(cur, "\n")
		if len(key) < minChars {
			return
		}
		blocks[key] = append(blocks[key], occurrence{file: path, line: start, n: len(cur)})
		if reason != "" {
			marked[key] = reason
		}
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
		body := strings.TrimSpace(strings.TrimPrefix(text, "//"))
		if m := markerRe.FindStringSubmatch(body); m != nil {
			// Strip unconditionally so a marked copy still hashes equal to
			// its unmarked twin; record the reason only when there is one,
			// so a bare marker is reported rather than obeyed.
			if r := strings.TrimSpace(m[1]); r != "" {
				reason = r
			}
			continue
		}
		cur = append(cur, wsRun.ReplaceAllString(body, " "))
	}
	flush()
	return blocks, marked, sc.Err()
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
