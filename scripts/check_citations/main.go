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
	"slices"
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
	"zero": 0, "nothing": 0,
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
	"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
}

// countRe reads a stated count out of the prose around a citation.
//
// The verb list is what the corpus uses, and the optional words between verb
// and number matter: the real phrasings include "finds exactly two lines",
// "now finds exactly two", "returns only one" and "returns exactly those two
// lines". Dropping "those"/"these" made the last shape unparseable, and the
// window then found a NEIGHBOURING citation's count instead — reporting a
// correct comment as wrong.
//
// A bare "no" is deliberately absent from the number words. "started and ended
// have no reader yet" is not a count claim, and reading it as zero flagged a
// true comment. "nothing" and "zero" stay, because both only appear as counts.
//
// The count can also PRECEDE the command — job.go's "There are five now —
// `grep ...`" — which is why the caller searches a window on both sides.
var countRe = regexp.MustCompile(
	`(?i)\b(?:finds|found|returns|return|lists|names|has|have|are|is)\s+` +
		`(?:exactly\s+|now\s+|only\s+|still\s+|just\s+|those\s+|these\s+|the\s+)*` +
		`(\d+|zero|nothing|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\b`)

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
	files, err := goFiles(root, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "check_citations: %v\n", err)
		os.Exit(2)
	}

	var bad, unverified, ok int
	for _, f := range files {
		if filepath.ToSlash(filepath.Dir(f)) == selfPkg {
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

func goFiles(root string, args []string) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}
	// Anchored to the root, not the process directory. git ls-files run from a
	// subdirectory both scopes the listing to that subdirectory and prints
	// paths relative to it, so an unanchored call silently skips every other
	// package AND makes main's filepath.Join(root, f) resolve to nothing.
	cmd := exec.Command("git", "ls-files", "*.go")
	cmd.Dir = root
	out, err := cmd.Output()
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
//
// Two passes, because a count's window has to be bounded by the citations
// around it and those are not known until every span is found.
func citationsIn(file string, b commentBlock) []citation {
	runes := []rune(b.text)

	type span struct{ open, end int }
	var spans []span
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
		if isCitationCmd(inner) {
			spans = append(spans, span{open: i, end: end})
		}
		i = end
	}

	out := make([]citation, 0, len(spans))
	for n, sp := range spans {
		// Bound the count's search at the NEIGHBOURING CITATIONS, not at any
		// backtick. internal/job/attempt.go carries two citations in one
		// comment block, each with its own count, and an unbounded window read
		// the first one's "returns exactly four lines" as the second's claim.
		// Clamping at every backtick instead was worse: prose routinely
		// backticks an identifier — scenario_test.go explains its pattern with
		// `q\.` immediately after the command — and that cut the real count off.
		lo, hi := 0, len(runes)
		if n > 0 {
			lo = spans[n-1].end + 1
		}
		if n+1 < len(spans) {
			hi = spans[n+1].open
		}
		inner := strings.TrimSpace(string(runes[sp.open+1 : sp.end]))
		line := 0
		if sp.open < len(b.lines) {
			line = b.lines[sp.open]
		}
		c := citation{file: file, line: line, cmd: inner}
		c.want, c.hasWant = statedCount(runes, sp.open, sp.end, lo, hi)
		out = append(out, c)
	}
	return out
}

// isCitationCmd reports whether backticked text is a command this tool runs,
// rather than one of the identifiers and signatures this codebase backticks
// constantly in prose.
func isCitationCmd(inner string) bool {
	return strings.HasPrefix(inner, "grep ") || strings.HasPrefix(inner, "git grep ")
}

// statedCount reads the count out of the prose on either side of the command.
//
// The window is bounded rather than the whole block because a long comment can
// easily contain an unrelated number; 240 characters is about two sentences,
// which is as far as a count and its command are ever separated in practice.
// The text after the command is searched FIRST — "finds four" follows the
// backticks in every case but one — and the text before is the fallback that
// catches job.go's "There are five now — `grep ...`".
//
// bound0 and bound1 are the neighbouring citations' edges, supplied by the
// caller: a count belongs to the citation it sits beside, and the prose of the
// next one is where this one's stops.
func statedCount(runes []rune, start, end, bound0, bound1 int) (int, bool) {
	const window = 240

	lo := max(max(start-window, 0), bound0)
	hi := min(min(end+1+window, len(runes)), bound1)

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

// stage is one grep invocation from a citation's pipeline, with its arguments
// already classified. Classification is what makes the rest safe: until the
// pattern is told apart from the file operands, no check can be correct — a
// metacharacter check hits regex anchors, and a glob expansion eats the
// pattern.
type stage struct {
	argv    []string // the command as it will be exec'd, before glob expansion
	gitGrep bool     // "git grep ..." rather than "grep ..."
	pattern string   // the search pattern
	paths   []string // file operands, if any
}

// runCitation executes one citation's pipeline and returns the match count and
// the matching lines.
func runCitation(root string, c citation) (matches int, lines []string, err error) {
	stages, err := parsePipeline(c.cmd)
	if err != nil {
		return 0, nil, err
	}
	cwd := citationDir(root, c, stages[0])
	if err := validateOperands(root, cwd, stages[0]); err != nil {
		return 0, nil, err
	}
	out, err := runStages(cwd, stages)
	if err != nil {
		return 0, nil, err
	}
	// TrimSuffix, not TrimRight: grep terminates every record with a newline,
	// so exactly one is a delimiter. TrimRight would also eat the output of a
	// citation matching blank lines, reporting a real count as zero.
	out = strings.TrimSuffix(out, "\n")
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
//
// Only the file OPERANDS are inspected. An earlier version looked at every
// non-flag argument, which meant a search pattern containing a slash — `// `,
// `path/filepath`, `http://` — silently selected the repository root, and a
// citation naming a file beside itself then ran where that file does not exist.
//
// git grep always runs from the root: it resolves pathspecs against the
// repository, not the process directory.
func citationDir(root string, c citation, st stage) string {
	if st.gitGrep {
		return root
	}
	for _, p := range st.paths {
		if strings.Contains(p, "/") {
			return root
		}
	}
	return filepath.Join(root, filepath.Dir(c.file))
}

// validateOperands is the containment check, and it exists because grep is
// itself a file-reading primitive.
//
// Refusing shell metacharacters stops a citation from RUNNING something else;
// it does nothing about a citation that runs grep exactly as intended against
// a file it should never see. `grep -a -n 'TOKEN' /proc/self/environ` is a
// legal grep, and on a mismatch this tool prints the matching lines — so
// without this, a comment in a branch could exfiltrate the environment of
// whoever runs the gate into their terminal or a CI log.
//
// So every operand must resolve inside the repository, and the pattern-file
// options are refused outright: -f reads its patterns from a path, which is a
// second file operand wearing a flag's clothes.
//
// A stage with no file operand at all is refused too. Go leaves a nil Stdin as
// os.DevNull, so a pathless grep reads EOF, exits 1, and is indistinguishable
// from an honest "returns nothing" — a citation that scans nothing while
// reporting a number.
func validateOperands(root, cwd string, st stage) error {
	if st.gitGrep {
		return nil // git grep resolves pathspecs against the repo; it cannot escape it
	}
	for _, a := range st.argv[1:] {
		if a == "-f" || a == "--file" || strings.HasPrefix(a, "--file=") {
			return fmt.Errorf("refusing to run: %s reads patterns from a file", a)
		}
	}
	if len(st.paths) == 0 {
		return errors.New("refusing to run: no file operand, so grep would read stdin and report zero")
	}
	for _, p := range st.paths {
		if filepath.IsAbs(p) {
			return fmt.Errorf("refusing to run: %q is an absolute path", p)
		}
		abs := filepath.Clean(filepath.Join(cwd, p))
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing to run: %q resolves outside the repository", p)
		}
	}
	return nil
}

// parsePipeline splits a citation into grep stages and refuses anything a
// shell would treat as more than that.
//
// The command is tokenized ONCE, for the whole pipeline, because every check
// below depends on knowing which characters were quoted. Splitting on "|"
// first — as an earlier version did — breaks any citation using regex
// alternation, since `grep -E 'foo|bar' x.go` becomes two stages, the first
// carrying an unbalanced quote.
func parsePipeline(cmd string) ([]stage, error) {
	toks, err := tokenize(cmd)
	if err != nil {
		return nil, err
	}
	var stages []stage
	var cur []token
	flush := func() error {
		if len(cur) == 0 {
			return errors.New("empty pipeline stage")
		}
		st, err := classify(cur)
		if err != nil {
			return err
		}
		stages = append(stages, st)
		cur = nil
		return nil
	}
	for _, t := range toks {
		if t.pipe {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		cur = append(cur, t)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return stages, nil
}

// classify turns one stage's tokens into argv plus the pattern/operand split,
// and applies the metacharacter refusal to UNQUOTED text only.
//
// Inside single quotes a shell expands nothing, so a regex anchor ('State$'),
// a channel receive ('<-ctx.Done()') and a redirect-looking character are all
// literal. Refusing them wholesale — which an earlier version did, by scanning
// the raw command string — rejected ordinary Go patterns as though they were
// injection attempts.
func classify(toks []token) (stage, error) {
	var st stage
	for _, t := range toks {
		if !t.quoted {
			if i := strings.IndexAny(t.text, shellMeta); i >= 0 {
				return st, fmt.Errorf("refusing to run: unquoted %q, which this tool does not interpret", t.text[i:i+1])
			}
		}
		st.argv = append(st.argv, t.text)
	}
	rest := st.argv
	switch {
	case rest[0] == "grep":
		rest = rest[1:]
	case rest[0] == "git" && len(rest) > 1 && rest[1] == "grep":
		st.gitGrep = true
		rest = rest[2:]
	default:
		return st, fmt.Errorf("refusing to run %q: only grep and git grep are allowed", rest[0])
	}

	seenPattern := false
	for _, a := range rest {
		if strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		if !seenPattern {
			st.pattern, seenPattern = a, true
			continue
		}
		st.paths = append(st.paths, a)
	}
	if !seenPattern {
		return st, errors.New("refusing to run: no search pattern")
	}
	return st, nil
}

// token is one shell word, remembering whether it was quoted, because every
// safety decision downstream turns on that.
type token struct {
	text   string
	quoted bool // any part of it was inside single quotes
	pipe   bool // an unquoted | separating two stages
}

// tokenize splits on whitespace and unquoted pipes, honouring single quotes.
//
// A double quote is refused only OUTSIDE single quotes: this tool does not
// implement double-quote semantics, but inside '...' a double quote is
// ordinary text, and a citation searching for a string literal needs it.
func tokenize(s string) ([]token, error) {
	var toks []token
	var cur strings.Builder
	inQuote, have, quoted := false, false, false
	flush := func() {
		if have {
			toks = append(toks, token{text: cur.String(), quoted: quoted})
			cur.Reset()
			have, quoted = false, false
		}
	}
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
			have, quoted = true, true
		case inQuote:
			cur.WriteRune(r)
			have = true
		case r == '"':
			return nil, errors.New("refusing to run: double quotes are not interpreted outside single quotes")
		case r == '|':
			flush()
			toks = append(toks, token{pipe: true})
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if inQuote {
		return nil, errors.New("refusing to run: unbalanced single quote")
	}
	flush()
	return toks, nil
}

// runStages executes the pipeline, expanding globs itself.
//
// grep exits 1 when nothing matched, which is a legitimate answer (a citation
// may state "returns nothing") and not an error. Exit 2 is a real failure, and
// its stderr is carried into the error: without it the report says only
// "exit status 2", which tells the reader nothing about what grep objected to.
func runStages(cwd string, stages []stage) (string, error) {
	var in string
	for i, st := range stages {
		expanded, err := expandArgs(cwd, st)
		if err != nil {
			return "", err
		}
		cmd := exec.Command(expanded[0], expanded[1:]...) // #nosec G204 -- argv is grep or git grep plus validated literals; see classify and validateOperands
		cmd.Dir = cwd
		if i > 0 {
			cmd.Stdin = strings.NewReader(in)
		}
		out, err := cmd.Output()
		var ee *exec.ExitError
		if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1) {
			if ee != nil && len(ee.Stderr) > 0 {
				return "", fmt.Errorf("%s: %w: %s", strings.Join(expanded, " "), err, strings.TrimSpace(string(ee.Stderr)))
			}
			return "", fmt.Errorf("%s: %w", strings.Join(expanded, " "), err)
		}
		in = string(out)
	}
	return in, nil
}

// expandArgs expands globs in the FILE OPERANDS, because no shell is doing it.
//
// Only the operands: an earlier version treated the first non-flag argument as
// a path whenever a flag preceded it, which is every citation in this
// repository. A regex pattern containing *, ? or [ was then globbed, and where
// it happened to match filenames the pattern was REPLACED by them — grep went
// looking for the contents of a filename. Nothing failed loudly.
//
// A pattern that matches nothing is left as written so grep reports it, rather
// than silently vanishing from the argv and turning a stale path into a
// zero-match result that looks like a legitimate answer.
func expandArgs(cwd string, st stage) ([]string, error) {
	if st.gitGrep {
		return st.argv, nil // git resolves its own pathspecs
	}
	var out []string
	for _, a := range st.argv {
		if !slices.Contains(st.paths, a) || !strings.ContainsAny(a, "*?[") {
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
