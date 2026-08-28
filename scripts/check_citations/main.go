// Command check_citations runs the `grep` commands that Go comments embed as
// evidence, and reports the ones whose stated count no longer matches reality.
//
// It exists because AGENTS.md's Standing Design Rule 4 requires a comment
// quantifying over a population — every writer, every caller, the one place —
// to state the enumeration that backs it, and because that class of claim is
// invisible to every other gate. Comments are neither type-checked nor
// executed: go build, go vet, golangci-lint and the whole test suite are
// structurally blind to a citation whose count has moved. check_dup_comments
// finds copies, not stale counts.
//
// The class is measured, not suspected. Seven of the twenty-two findings on
// the Half A review rounds were citation claims, and PR #448 added five more —
// a call-site list naming three stale line numbers, a pattern that matched its
// own function declaration, a count of six that had become eight. Every
// enumeration in this repository that became a TEST has stayed true; every one
// that stayed prose has gone stale at least once.
//
// One failure shape motivates the tool more than the others, because no local
// reading can catch it: a comment in one file can falsify a citation in
// another. internal/job/job.go cites `grep -n 'q\.mu\.Lock' internal/sched/*.go`
// and states a count. That citation was correct when written and was broken
// later by prose added to internal/sched/queue.go which happened to contain an
// unescaped q.mu.Lock() — a file the job.go author never touched. Nothing
// short of running the command finds that.
//
// # What it checks
//
// A citation is a backtick-quoted command inside a // comment whose first word
// is grep. The command may wrap across comment lines, including mid-token, so
// the lines are rejoined before parsing. The count is read from the prose
// around it: "finds four", "returns exactly one line", "returns nothing".
//
// A stated count that disagrees with the command's real output is an error. A
// citation whose count cannot be parsed is reported as unverified and does not
// fail the run — the tool is meant to be adoptable against comments written
// before it existed, and tightening that is a separate decision.
//
// # Why it skips its own package
//
// This program's own comments quote citation syntax in order to document it,
// so they contain greps that are illustrations rather than claims — the doc
// comment above cites internal/sched paths it has no relationship to, and the
// tests describe wrapped commands as literal examples. Scanning them reports
// prose ABOUT the convention as though it were prose USING it.
//
// That is a property of this one package, not a general escape hatch: nowhere
// else in the repository does a comment quote a grep it does not mean. If a
// second such file ever appears, an explicit marker in the //dupcomment:ok
// mould is the right answer; adding one now would be a mechanism with a single
// user, and the user is the tool itself.
//
// The gap was invisible until these files were committed, because git ls-files
// does not list untracked files — so every run during development scanned a
// tree that did not yet contain them.
//
// # Why it never uses a shell
//
// The commands come from source comments, so running them through sh would
// make every comment in the repository a code-execution surface — including in
// a vendored dependency or a contributor's first patch. Commands are parsed
// into argv and grep is exec'd directly. Anything a shell would interpret
// beyond a pipe between greps — ;, &&, $(), redirection — is refused and
// reported rather than run. Globs are expanded by this program, not by a
// shell, for the same reason.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// citation is one backticked grep command found in a comment, with whatever
// count the surrounding prose stated for it.
type citation struct {
	file    string
	line    int    // 1-based source line the command's opening backtick sits on
	cmd     string // the command text, rejoined across comment lines
	want    int
	hasWant bool
}

// numWords covers the range that actually appears in this repository's
// citations. Deliberately small: a citation stating a count above twelve is
// almost certainly counting something a test should own instead, and silently
// parsing it would hide that.
var numWords = map[string]int{
	"zero": 0, "no": 0, "nothing": 0,
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
}

// countRe reads a stated count out of the prose around a citation.
//
// The verb list is what the corpus uses, and the optional adverbs matter: the
// real phrasings include "finds exactly two lines", "now finds exactly two"
// and "returns only one". The count can also PRECEDE the command — job.go's
// "There are five now — `grep ...`" — which is why the caller searches a
// window on both sides rather than only forward.
var countRe = regexp.MustCompile(
	`(?i)\b(?:finds|found|returns|return|lists|names|has|have|are|is)\s+` +
		`(?:exactly\s+|now\s+|only\s+|still\s+|just\s+)*` +
		`(\d+|zero|no|nothing|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\b`)

// selfPkg is this program's own directory; see "Why it skips its own package".
const selfPkg = "scripts/check_citations"

// shellMeta are the characters this tool refuses to run rather than interpret.
// A backtick cannot appear (it delimits the citation) and is listed anyway so
// the refusal does not depend on that remaining true.
const shellMeta = ";&$><\n\r`"

func main() {
	verbose := flag.Bool("v", false, "list every citation, including the ones that agree")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "check_citations: %v\n", err)
		os.Exit(2)
	}
	files, err := goFiles(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "check_citations: %v\n", err)
		os.Exit(2)
	}

	var bad, unverified, ok int
	for _, f := range files {
		if filepath.Dir(f) == selfPkg {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, f)) //nolint:gosec // G304: paths come from git ls-files or argv
		if err != nil {
			fmt.Fprintf(os.Stderr, "check_citations: %s: %v\n", f, err)
			os.Exit(2)
		}
		for _, c := range extract(f, string(src)) {
			got, lines, err := runCitation(root, c)
			if err != nil {
				bad++
				fmt.Printf("%s:%d: citation cannot be run: %v\n    %s\n", c.file, c.line, err, c.cmd)
				continue
			}
			if !c.hasWant {
				unverified++
				if *verbose {
					fmt.Printf("%s:%d: no count stated, ran clean (%d matches)\n    %s\n", c.file, c.line, got, c.cmd)
				}
				continue
			}
			if got != c.want {
				bad++
				fmt.Printf("%s:%d: citation states %d, command finds %d\n    %s\n",
					c.file, c.line, c.want, got, c.cmd)
				for _, l := range lines {
					fmt.Printf("        %s\n", l)
				}
				continue
			}
			ok++
			if *verbose {
				fmt.Printf("%s:%d: ok (%d)\n    %s\n", c.file, c.line, got, c.cmd)
			}
		}
	}

	fmt.Printf("Status: %d citations agree, %d unverified (no count stated), %d wrong.\n", ok, unverified, bad)
	if bad > 0 {
		os.Exit(1)
	}
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

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

// commentBlock is a run of consecutive // lines, rejoined into one string so a
// command that wraps across lines can be read as a unit.
type commentBlock struct {
	text  string
	lines []int // source line number for each rune offset's originating line
}

// extract finds every citation in one file's source.
//
// Comment lines are rejoined with a single space rather than a newline,
// because the corpus wraps commands mid-token: `grep -n` on one line and
// `'q\.mu\.Lock' internal/sched/*.go | grep -v` on the next. Rejoining with a
// space reconstructs exactly the command the author wrote.
func extract(file, src string) []citation {
	var out []citation
	lines := strings.Split(src, "\n")

	for i := 0; i < len(lines); i++ {
		if !isCommentLine(lines[i]) {
			continue
		}
		j := i
		var b strings.Builder
		var origin []int
		for ; j < len(lines) && isCommentLine(lines[j]); j++ {
			body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[j]), "//"))
			if b.Len() > 0 {
				b.WriteByte(' ')
				origin = append(origin, j+1)
			}
			for range body {
				origin = append(origin, j+1)
			}
			b.WriteString(body)
		}
		out = append(out, citationsIn(file, commentBlock{text: b.String(), lines: origin})...)
		i = j
	}
	return out
}

func isCommentLine(l string) bool {
	return strings.HasPrefix(strings.TrimSpace(l), "//")
}

// citationsIn pulls every backticked grep command out of one rejoined block.
func citationsIn(file string, b commentBlock) []citation {
	var out []citation
	runes := []rune(b.text)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '`' {
			continue
		}
		end := -1
		for k := i + 1; k < len(runes); k++ {
			if runes[k] == '`' {
				end = k
				break
			}
		}
		if end < 0 {
			break // unterminated backtick; nothing further in this block is trustworthy
		}
		inner := strings.TrimSpace(string(runes[i+1 : end]))
		i = end
		if !strings.HasPrefix(inner, "grep ") {
			continue // a backticked identifier or signature, not a citation
		}
		line := 0
		if i < len(b.lines) {
			line = b.lines[i]
		}
		c := citation{file: file, line: line, cmd: inner}
		c.want, c.hasWant = statedCount(string(runes), i-len(inner), end)
		out = append(out, c)
	}
	return out
}

// statedCount reads the count out of the prose on either side of the command.
//
// The window is bounded rather than the whole block because a long comment can
// easily contain an unrelated number; 240 characters is about two sentences,
// which is as far as a count and its command are ever separated in practice.
// The text after the command is searched FIRST — "finds four" follows the
// backticks in every case but one — and the text before is the fallback that
// catches job.go's "There are five now — `grep ...`".
func statedCount(block string, start, end int) (int, bool) {
	const window = 240
	runes := []rune(block)

	lo := max(start-window, 0)
	hi := min(end+1+window, len(runes))

	if n, okAfter := firstCount(string(runes[min(end+1, len(runes)):hi])); okAfter {
		return n, true
	}
	if n, okBefore := lastCount(string(runes[lo:max(start, 0)])); okBefore {
		return n, true
	}
	return 0, false
}

func firstCount(s string) (int, bool) {
	m := countRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	return wordToNum(m[1])
}

func lastCount(s string) (int, bool) {
	ms := countRe.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return 0, false
	}
	return wordToNum(ms[len(ms)-1][1])
}

func wordToNum(w string) (int, bool) {
	if n, err := strconv.Atoi(w); err == nil {
		return n, true
	}
	n, ok := numWords[strings.ToLower(w)]
	return n, ok
}

// runCitation executes one citation's pipeline and returns the match count and
// the matching lines.
func runCitation(root string, c citation) (matches int, lines []string, err error) {
	stages, err := parsePipeline(c.cmd)
	if err != nil {
		return 0, nil, err
	}
	cwd := citationDir(root, c, stages[0])
	out, err := runStages(cwd, stages)
	if err != nil {
		return 0, nil, err
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return 0, nil, nil
	}
	ls := strings.Split(out, "\n")
	return len(ls), ls, nil
}

// citationDir picks the directory a citation's paths are relative to.
//
// Two conventions are in use and both are legitimate: a path containing a
// separator (internal/sched/*.go) is written from the repository root, and a
// bare filename (advance.go) is written from the citing file's own directory,
// which is how it reads to someone with that file open.
func citationDir(root string, c citation, first []string) string {
	for _, a := range first[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if strings.Contains(a, "/") {
			return root
		}
	}
	return filepath.Join(root, filepath.Dir(c.file))
}

// parsePipeline splits a citation into grep stages and refuses anything a
// shell would treat as more than that.
func parsePipeline(cmd string) ([][]string, error) {
	if i := strings.IndexAny(cmd, shellMeta); i >= 0 {
		return nil, fmt.Errorf("refusing to run: contains %q, which this tool does not interpret", cmd[i:i+1])
	}
	var stages [][]string
	for seg := range strings.SplitSeq(cmd, "|") {
		argv, err := tokenize(strings.TrimSpace(seg))
		if err != nil {
			return nil, err
		}
		if len(argv) == 0 {
			return nil, errors.New("empty pipeline stage")
		}
		if argv[0] != "grep" {
			return nil, fmt.Errorf("refusing to run %q: only grep is allowed", argv[0])
		}
		stages = append(stages, argv)
	}
	return stages, nil
}

// tokenize splits on whitespace, honouring single quotes. The corpus quotes
// patterns with single quotes only; a double quote is refused rather than
// guessed at, because guessing wrong changes which pattern gets run.
func tokenize(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inQuote, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			return nil, errors.New(`refusing to run: double quotes are not interpreted`)
		case r == '\'':
			inQuote = !inQuote
			have = true
		case !inQuote && (r == ' ' || r == '\t'):
			if have {
				argv = append(argv, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if inQuote {
		return nil, errors.New("refusing to run: unbalanced single quote")
	}
	if have {
		argv = append(argv, cur.String())
	}
	return argv, nil
}

// runStages executes the pipeline, expanding globs itself.
//
// grep exits 1 when nothing matched, which is a legitimate answer (a citation
// may state "returns nothing") and not an error. Exit 2 is a real failure.
func runStages(cwd string, stages [][]string) (string, error) {
	var in string
	for i, argv := range stages {
		expanded, err := expandArgs(cwd, argv)
		if err != nil {
			return "", err
		}
		cmd := exec.Command(expanded[0], expanded[1:]...) // #nosec G204 -- argv is grep plus validated literals; see parsePipeline
		cmd.Dir = cwd
		if i > 0 {
			cmd.Stdin = strings.NewReader(in)
		}
		out, err := cmd.Output()
		var ee *exec.ExitError
		if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1) {
			return "", fmt.Errorf("%s: %w", strings.Join(expanded, " "), err)
		}
		in = string(out)
	}
	return in, nil
}

// expandArgs expands a glob in a path argument, because no shell is doing it.
//
// A pattern that matches nothing is left as written so grep reports it, rather
// than silently vanishing from the argv and turning a stale path into a
// zero-match result that looks like a legitimate answer.
func expandArgs(cwd string, argv []string) ([]string, error) {
	out := []string{argv[0]}
	for i, a := range argv[1:] {
		isPath := i > 0 && !strings.HasPrefix(a, "-")
		if !isPath || !strings.ContainsAny(a, "*?[") {
			out = append(out, a)
			continue
		}
		matches, err := filepath.Glob(filepath.Join(cwd, a))
		if err != nil {
			return nil, fmt.Errorf("bad glob %q: %w", a, err)
		}
		if len(matches) == 0 {
			out = append(out, a)
			continue
		}
		for _, m := range matches {
			rel, err := filepath.Rel(cwd, m)
			if err != nil {
				return nil, err
			}
			out = append(out, rel)
		}
	}
	return out, nil
}
