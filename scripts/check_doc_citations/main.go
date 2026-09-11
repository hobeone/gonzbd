// Command check_doc_citations reports references in prose that no longer
// resolve: a file path that names nothing in the tree, a Test function cited
// as a guard that is not declared anywhere, and a "doc.md § Section" citation
// naming a heading that document does not have.
//
// It exists because the repository's existing gates are structurally blind to
// this. check_citations runs `git ls-files *.go` and has never opened a
// Markdown file, so docs/*.md was checked by nothing at all; and within Go it
// only executes comments that EMBED a grep command, which is a small minority
// of the claims a comment makes. go build, go vet, golangci-lint and the test
// suite are blind to every one of these, because comments and Markdown are
// neither type-checked nor executed.
//
// # The class is measured, not suspected
//
// The pass that motivated this tool found, in a single sweep:
//
//   - internal/queue was deleted in b6651d43 and dissolved into internal/job,
//     internal/sched and internal/dispatch. Roughly 400 references to it
//     survived across 31 files, naming 30 specific source files that are not
//     in the tree. Every comment sweep run in the interim grepped those files
//     and read them as though they described the code.
//   - docs cited five migrations — 002_add_jobs_tables, 008, 009, 010, 011 —
//     that no longer exist. They were real and shipped, and were discarded by
//     the 001-011 collapse; the docs went on citing them by name for months.
//     The durability contract's supersession ledger was anchored on files the
//     tree no longer had. What replaced them has since been collapsed in turn,
//     so the schema is again a single 001_initial.sql.
//   - Five tests were cited as the guard on an invariant while not existing.
//     One of them, TestSeedFromRuns_StaysAdditive, was named three times
//     across two documents as one of "the only tests in the repository that
//     redden when the two entry points are merged".
//
// The last shape is the reason this is a gate rather than a lint. AGENTS.md's
// Standing Design Rule 4 makes the point: a wrong enumeration "does not merely
// fail to help, it actively stops the check it replaced". A reader who sees a
// named test does not go looking for one.
//
// # What it checks
//
// Three independent checks over every tracked .md file and every // comment in
// every tracked .go file.
//
// PATHS. A token that looks like a repository-relative path — it contains a
// slash and ends in one of the extensions this repository actually uses — must
// resolve to something in the tree. Tokens are taken from backtick spans and
// from bare prose alike, because a stale path is equally wrong either way.
// Absolute paths, URLs and parent-relative escapes are ignored: they name
// things outside the tree, which this tool cannot and should not adjudicate.
//
// TESTS. A token matching Test[A-Z]... must be a declared test function
// somewhere in the tree, OR be a proper prefix of one. The prefix rule is not
// a loophole; it is what makes the check usable. Prose legitimately refers to
// a FAMILY of tests by its shared stem — "TestAdvance covers §3.6" where the
// declarations are TestAdvance_BranchOne_StartsANeverRunJob and its siblings —
// and flagging that would produce noise with no defect behind it.
//
// SECTIONS. A citation naming both a document and a section in it —
// "docs/go-standards.md § Concurrency & Locking" — must name a heading that
// document actually has. This was added after two such citations were found
// pointing at nothing, one of which had survived every gate since 0bbb5fc5
// moved the section it named into another file. Path and Test citations were
// checked; the half of the reference that names a heading was not, so
// "AGENTS.md § <anything>" passed for as long as AGENTS.md existed.
//
// Only the form that names its target is checked. A bare "§3.4" is not
// resolvable: it names no document, and 3.4 is a real heading in
// docs/post_processing_spec.md and docs/sabnzbd_spec.md while
// docs/durability-contract.md numbers its invariants flatly, so picking a
// target would mean guessing. 320 bare refs across 100 files were surveyed
// when this was written and none is inspected, because a guess that lands on
// the wrong document reports a correct citation as broken.
//
// # Why identifiers in general are NOT checked
//
// The obvious generalisation — every backticked CamelCase token must exist in
// *.go — was built, measured, and rejected. Against this repository it flagged
// roughly 120 tokens of which about 80% were correct as written:
//
//   - docs/sabnzbd_spec.md and docs/post_processing_spec.md document the
//     PYTHON reference implementation. PENALTY_UNKNOWN, NzbFile, Popen and
//     PYTHONUNBUFFERED are supposed to be absent from Go.
//   - GREMLINS_WORKERS, GREMLINS_DISK_MAX_MB and friends are environment
//     variables; PUID, PGID and GONZBD_PORT likewise.
//   - S1008 is a staticcheck code, HeapAlloc a runtime.MemStats field,
//     slog.TextHandler stdlib, Killed and Lived gremlins output tokens.
//   - AGENTS.md's TestTheNewPin is a placeholder the document explicitly
//     labels as naming no test.
//
// A gate that is wrong four times in five is not a gate; it is a thing people
// learn to skip, and it would have discredited the two checks above by
// association. The narrow checks earn their failures.
//
// # Exemptions
//
// Prose sometimes names a deleted thing ON PURPOSE — "the test that stood
// here, X, was folded into Y" is good documentation, and rewriting it to
// avoid naming X would destroy the sentence's point.
//
//	//doccite:ok TestFoo — folded into TestBar in #451
//	<!-- doccite:ok TestFoo — folded into TestBar in #451 -->
//
// The marker exempts that one token within that one file. A reason is
// mandatory, in the mould of //nocover:, //dupcomment:ok and
// //testdouble:allow: a bare marker is itself an error, because an exemption
// nobody had to justify is indistinguishable from one nobody thought about.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// pathRE matches a repository-relative path: at least one slash, ending in an
// extension this repository uses. Deliberately NOT anchored on backticks —
// a stale path in bare prose is exactly as wrong as one in a code span, and
// docs/*.md states paths both ways.
//
// The leading [.~/] alternation is load-bearing rather than tidy. Without it
// \b starts the match AFTER a leading dot or tilde, so `~/.claude/CLAUDE.md`
// matches as `claude/CLAUDE.md` and `.github/workflows/ci.yml` as
// `github/workflows/ci.yml` — both then fail to resolve, and the tool reports
// a defect it invented by mis-tokenising. Capturing the prefix lets skipPath
// see the real shape and drop the ones that name something outside the tree.
var pathRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./~-])([.~/]*(?:[A-Za-z0-9_.+-]+/)+[A-Za-z0-9_.+-]+\.(?:go|md|sql|sh|ya?ml|ts|svelte|json))\b`)

// testRE matches a Go test function name as prose would write it. The {4,}
// tail keeps it off short CamelCase words that merely begin with "Test".
var testRE = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]{4,}\b`)

// sectionRE matches a citation that names BOTH a document and a section in it:
// "docs/go-standards.md § Concurrency & Locking", with or without backticks
// and with or without quotes around the section name.
//
// Only this form is checkable. A bare "§3.4" names no document, and the
// numbering is not unique across the tree — 3.4 and 3.5 are real headings in
// docs/post_processing_spec.md and docs/sabnzbd_spec.md while
// docs/durability-contract.md numbers its invariants flatly — so resolving one
// would mean guessing which document the author meant. 320 bare refs across
// 100 files were surveyed when this check was written; none is inspected here,
// deliberately, because a guess that lands on the wrong document reports a
// correct citation as broken.
var sectionRE = regexp.MustCompile(
	"(?:^|[^A-Za-z0-9_./~-])([.~/]*(?:[A-Za-z0-9_.+-]+/)*[A-Za-z0-9_.+-]+\\.md)`?\\s*§\\s*(?:\"([^\"]{2,60})\"|`?([A-Z][A-Za-z0-9_&'` -]{2,60}))")

// headingWords normalises one heading or one cited section name to lowercase
// words, dropping a leading section number and any markup. "### 1. Concurrency
// & Locking" and "§ Concurrency & Locking" both become [concurrency & locking].
func headingWords(s string) []string {
	s = strings.TrimLeft(s, "#")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "*", "")
	s = strings.TrimSpace(s)
	// Drop a leading "1." / "10.4" / "5.B" section number.
	if i := strings.IndexByte(s, ' '); i > 0 {
		if first := s[:i]; strings.IndexFunc(first, func(r rune) bool {
			return (r < '0' || r > '9') && r != '.' && (r < 'A' || r > 'Z')
		}) == -1 && strings.ContainsAny(first, "0123456789") {
			s = s[i+1:]
		}
	}
	// Trailing punctuation is stripped per word so that a heading which
	// continues past its name — "## The tick: a ticker owns liveness" — still
	// matches a citation of the name alone, "§ The tick". Without this the
	// comparison comes down to "tick:" against "tick" and reports a live
	// reference as broken; that is how this gate's own first citation failed.
	words := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	for i, w := range words {
		if t := strings.TrimRight(w, ":;,."); t != "" {
			words[i] = t
		}
	}
	return words
}

// headingCache memoises the headings of each target document.
var headingCache = map[string][][]string{}

// docHeadings returns every ATX heading in rel, normalised by headingWords.
func docHeadings(root, rel string) [][]string {
	if hs, ok := headingCache[rel]; ok {
		return hs
	}
	var hs [][]string
	if body, err := os.ReadFile(filepath.Join(root, rel)); err == nil { //nolint:gosec // G304: rel resolved inside root by statInside
		for line := range strings.SplitSeq(string(body), "\n") {
			if strings.HasPrefix(line, "#") {
				if w := headingWords(line); len(w) > 0 {
					hs = append(hs, w)
				}
			}
		}
	}
	headingCache[rel] = hs
	return hs
}

// sectionResolves reports whether the cited name matches a heading in the
// target, comparing the first TWO normalised words.
//
// Two, not the whole string, because an unquoted citation runs into the
// sentence around it — "AGENTS.md § Standing Design Rule 4 states the rule"
// cites the section "Standing Design Rules" and carries four words of prose
// past it, and a citation is routinely a prefix of a longer heading
// ("§ The red check" for "The red check — why it must be observed"). Two words
// is enough to separate a section that exists from one that does not, which is
// the only question this gate asks; it deliberately does not police whether
// the rest of the name is current.
func sectionResolves(root, citing, target, name string) bool {
	rel := target
	if !statInside(root, rel) {
		dir := filepath.Dir(citing)
		for dir != "." && dir != string(filepath.Separator) {
			if statInside(root, filepath.Join(dir, target)) {
				rel = filepath.Join(dir, target)
				break
			}
			dir = filepath.Dir(dir)
		}
	}
	if !statInside(root, rel) {
		// The document itself is missing; pathRE already reports that, and
		// reporting the section too would double-count one defect.
		return true
	}
	want := headingWords(name)
	if len(want) == 0 {
		return true
	}
	n := min(len(want), 2)
	for _, h := range docHeadings(root, rel) {
		if len(h) < n {
			continue
		}
		if slices.Equal(h[:n], want[:n]) {
			return true
		}
	}
	return false
}

// declRE matches a test declaration. Benchmarks, fuzz targets and examples
// count: prose cites them the same way and they fail the same way.
//
// The optional receiver group is not about tests at all. This repository has
// production METHODS whose names begin with Test — AppServices.TestNNTPServer
// and AppServices.TestDownloadDirWriteSpeedMBPerSec, which test a configured
// server and a disk on the user's behalf. They are real declared symbols that
// prose cites by name, so a pattern requiring no receiver reports every such
// mention as a missing test.
var declRE = regexp.MustCompile(`^func (?:\([^)]*\)\s*)?((?:Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]*)\s*\(`)

// ifaceMethodRE matches a Test-prefixed method in an interface body, which has
// no func keyword at all. AppServices declares both of the above that way.
var ifaceMethodRE = regexp.MustCompile(`^\s*((?:Test)[A-Za-z0-9_]*)\s*\([^)]*\)`)

// obituaryRE matches prose that is deliberately naming something GONE.
//
// This is the difference between a stale citation and good documentation, and
// it is worth recognising rather than taxing. "The test that stood here,
// TestFailToleratesAnEmptyMessageID, translated a Message-ID contract that no
// longer exists" is a sentence whose whole point is the name it mentions;
// rewriting it to avoid the name would destroy it, and demanding a marker on
// top of prose that already explains itself is ceremony.
//
// The vocabulary is the signal. An author who writes "was here", "used to be
// named" or "folded into" has demonstrably noticed the thing is absent — which
// is exactly what a stale citation's author had not. Twenty comments in this
// repository match; every one was checked by hand and every one was
// deliberate.
//
// It is scoped to the citing line and the two before it, so that a passage
// describing one removal cannot license an unrelated stale name six lines
// down.
var obituaryRE = regexp.MustCompile(`(?i)\b(was here|stood here|used to be|used to name|previously named|since deleted|no longer exists|folded into|replaces|replaced by|translates|there was a|went with|has no successor|deleted|removed)\b`)

// markerRE matches an exemption and captures the token and the reason. The
// reason group is required to be non-empty by the caller, not by the pattern,
// so that a bare marker can be reported as its own error rather than simply
// failing to match and being silently ignored.
var markerRE = regexp.MustCompile(`doccite:ok\s+([A-Za-z0-9_./-]+)\s*(.*)`)

type finding struct {
	file, token, kind string
	line              int
}

func main() {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "check_doc_citations:", err)
		os.Exit(2)
	}

	mdFiles, err := tracked(root, "*.md")
	if err != nil {
		fmt.Fprintln(os.Stderr, "check_doc_citations:", err)
		os.Exit(2)
	}
	goFiles, err := tracked(root, "*.go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "check_doc_citations:", err)
		os.Exit(2)
	}

	tests, err := declaredTests(root, goFiles)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check_doc_citations:", err)
		os.Exit(2)
	}

	var findings []finding
	var bareMarkers []finding

	scan := func(file string, goSource bool) {
		// CLAUDE.md and GEMINI.md are symlinks to AGENTS.md, so scanning them
		// reports every finding in that file three times over. git ls-files
		// lists all three; Lstat is what tells them apart.
		if fi, err := os.Lstat(filepath.Join(root, file)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return
		}
		body, err := os.ReadFile(filepath.Join(root, file)) //nolint:gosec // G304: paths come from git ls-files
		if err != nil {
			return
		}
		// A frozen record under docs/reviews/ describes the tree at a named
		// commit and says so in a banner check_review_banner already enforces.
		// Its paths are SUPPOSED to be the ones that existed then; correcting
		// them would falsify the record, which is the opposite of the point.
		//
		// Gated on the DIRECTORY as well as the phrase. Matching the phrase
		// alone silently exempted AGENTS.md, which contains it only because it
		// documents the banner convention — a file that is emphatically not a
		// frozen record, and one of the two this tool most needs to check.
		if !goSource && strings.HasPrefix(file, "docs/reviews/") &&
			strings.Contains(strings.ToLower(string(body)), "frozen record") {
			return
		}
		exempt, bare := exemptions(file, string(body))
		bareMarkers = append(bareMarkers, bare...)
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if goSource && !isComment(line) {
				continue
			}
			if strings.Contains(line, "doccite:ok") {
				continue
			}
			for _, m := range pathRE.FindAllStringSubmatch(line, -1) {
				p := m[1]
				if exempt[p] || skipPath(p) {
					continue
				}
				if resolves(root, file, p) || isObituary(lines, i) {
					continue
				}
				findings = append(findings, finding{file: file, line: i + 1, token: p, kind: "path"})
			}
			for _, t := range testRE.FindAllString(line, -1) {
				if exempt[t] || tests[t] || isPrefixOfDeclared(t, tests) {
					continue
				}
				if isObituary(lines, i) {
					continue
				}
				findings = append(findings, finding{file: file, line: i + 1, token: t, kind: "test"})
			}
			for _, m := range sectionRE.FindAllStringSubmatch(line, -1) {
				target, name := m[1], m[2]
				if name == "" {
					name = m[3]
				}
				tok := target + " § " + name
				if exempt[target] || exempt[name] || skipPath(target) {
					continue
				}
				if sectionResolves(root, file, target, name) || isObituary(lines, i) {
					continue
				}
				findings = append(findings, finding{file: file, line: i + 1, token: tok, kind: "section"})
			}
		}
	}

	for _, f := range mdFiles {
		scan(f, false)
	}
	for _, f := range goFiles {
		// This package's own doc comment quotes stale paths and test names as
		// worked examples of the defect. Scanning it reports prose ABOUT the
		// convention as prose USING it — the same carve-out, and for the same
		// reason, that check_citations makes for itself.
		if strings.HasPrefix(f, "scripts/check_doc_citations/") {
			continue
		}
		scan(f, true)
	}

	report(findings, bareMarkers)
}

// report prints findings grouped by file and exits non-zero if any exist.
func report(findings, bare []finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})

	for _, b := range bare {
		fmt.Printf("%s:%d: doccite:ok %s has no reason — state why the reference is deliberate\n",
			b.file, b.line, b.token)
	}
	for _, f := range findings {
		switch f.kind {
		case "path":
			fmt.Printf("%s:%d: path %s does not exist\n", f.file, f.line, f.token)
		case "test":
			fmt.Printf("%s:%d: %s is cited but no such test is declared\n", f.file, f.line, f.token)
		case "section":
			fmt.Printf("%s:%d: %s — that document has no such heading\n", f.file, f.line, f.token)
		}
	}

	if len(findings) == 0 && len(bare) == 0 {
		fmt.Println("Status: every cited path resolves, every cited test is declared, " +
			"and every cited section names a real heading.")
		return
	}
	fmt.Printf("\nStatus: %d unresolved reference(s), %d unjustified exemption(s).\n",
		len(findings), len(bare))
	fmt.Println("Correct the reference, or mark it deliberate with a reason:")
	fmt.Println("  //doccite:ok <token> — <why this names something absent>")
	os.Exit(1)
}

// exemptions collects doccite:ok markers in a file, returning the exempt
// tokens and any marker that carries no reason.
func exemptions(file, body string) (map[string]bool, []finding) {
	exempt := make(map[string]bool)
	var bare []finding
	for i, line := range strings.Split(body, "\n") {
		m := markerRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		token := m[1]
		reason := strings.Trim(m[2], " \t-—:>*/")
		if reason == "" {
			bare = append(bare, finding{file: file, line: i + 1, token: token})
			continue
		}
		exempt[token] = true
	}
	return exempt, bare
}

// isObituary reports whether the citing line or the two before it are
// deliberately describing something that is gone. See obituaryRE.
func isObituary(lines []string, i int) bool {
	// Both directions. Prose puts the explanation either side of the name —
	// "TestFoo was folded into TestBar" reads backward, while "the pin that
	// held this, TestFoo, went with internal/queue" puts the vocabulary on the
	// NEXT line once the comment wraps.
	for j := max(0, i-2); j <= min(len(lines)-1, i+2); j++ {
		if obituaryRE.MatchString(lines[j]) {
			return true
		}
	}
	return false
}

// isPrefixOfDeclared reports whether tok names a family of tests rather than
// one — "TestAdvance" where TestAdvance_BranchOne_StartsANeverRunJob exists,
// or "TestAllFlat" where docs give it to `go test -run` and
// TestAllFlatConfigTagsAreSettable is what runs.
//
// Any prefix counts, not only one ending at an underscore. The stricter rule
// was tried first, on the reasoning that TestFoo should not license TestFooBar
// — but `-run` takes an unanchored regex, so a bare stem is how the
// documentation is SUPPOSED to cite a family, and the strict rule flagged
// every such line. It costs little: the citations this tool exists to catch
// were TestSeedFromRuns_StaysAdditive, TestJobPhase_EveryStatusIsMapped-
// Deliberately and TestAllFetchPolicies_Exhaustive, none of which is a prefix
// of any declared test, so all three are still caught.
func isPrefixOfDeclared(tok string, tests map[string]bool) bool {
	for name := range tests {
		if len(name) > len(tok) && strings.HasPrefix(name, tok) {
			return true
		}
	}
	return false
}

// resolves reports whether p names something real, read from where the citing
// file sits rather than only from the repository root.
//
// Prose abbreviates. internal/dispatch/registry.go says "job/intent.go", not
// "internal/job/intent.go"; internal/rarheader's test says "testdata/
// ATTRIBUTION.md". Both are unambiguous to a reader and both fail a
// root-relative Stat. Walking up from the citing file's directory resolves
// them the way the reader does — and it stays strict where it matters, since
// internal/queue/queue.go resolves from no ancestor of anything.
func resolves(root, citing, p string) bool {
	if statInside(root, p) {
		return true
	}
	dir := filepath.Dir(citing)
	for dir != "." && dir != string(filepath.Separator) {
		if statInside(root, filepath.Join(dir, p)) {
			return true
		}
		dir = filepath.Dir(dir)
	}
	return false
}

// statInside reports whether rel names something that exists AND sits inside
// root. The containment check is not ceremony: rel is a token lifted out of
// prose, so a comment could name "a/../../../etc/passwd", which filepath.Join
// cleans into an escape. skipPath already drops a LEADING "../", but not one
// buried mid-path.
//
// Only existence is ever tested — this tool opens nothing it finds this way —
// so the worst an escape could do is disclose whether a path exists outside
// the tree. That is a small leak and this makes it none, which is the same
// standard check_citations holds itself to when it refuses to run a comment's
// grep through a shell.
func statInside(root, rel string) bool {
	full := filepath.Clean(filepath.Join(root, rel))
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return false
	}
	_, err := os.Stat(full) //nolint:gosec // G703: contained by the check above; existence only, never read
	return err == nil
}

// skipPath drops tokens that name something outside the tree, which this tool
// has no standing to judge: absolute paths, home-relative paths, URLs, module
// paths, and parent-relative escapes. ~/.claude/CLAUDE.md and
// ~/.config/gonzbd/gonzbd.yaml are real files on a developer's machine and
// their absence from the repository is the point, not a defect.
func skipPath(p string) bool {
	// A slash-joined LIST of two files ("go_sevenzip.go/go_unrar.go") is not a
	// path, and neither is a shell variable standing in for a directory
	// ("$HOME/..." reaches here as "HOME/..." once the sigil is trimmed).
	if strings.Count(p, ".go/") > 0 || strings.HasPrefix(p, "HOME/") {
		return true
	}
	// Standard-library and toolchain paths name files in GOROOT.
	for _, std := range []string{"syscall/", "runtime/", "os/exec/", "net/http/"} {
		if strings.HasPrefix(p, std) {
			return true
		}
	}
	return strings.HasPrefix(p, "/") ||
		strings.HasPrefix(p, "~") ||
		strings.HasPrefix(p, "../") ||
		strings.Contains(p, "://") ||
		strings.HasPrefix(p, "github.com/") ||
		strings.HasPrefix(p, "golang.org/") ||
		strings.HasPrefix(p, "gopkg.in/") ||
		strings.HasPrefix(p, "modernc.org/")
}

// isComment reports whether a line is a // comment. Deliberately line-based
// rather than a full parse: /* */ blocks are vanishingly rare in this
// repository, and a parser would make the tool refuse to run on a tree that
// does not compile — which is exactly when a sweep is most useful.
func isComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "//")
}

// declaredTests returns every test-like function declared in the tree.
func declaredTests(root string, goFiles []string) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, f := range goFiles {
		body, err := os.ReadFile(filepath.Join(root, f)) //nolint:gosec // G304: paths come from git ls-files
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		s := bufio.NewScanner(strings.NewReader(string(body)))
		s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for s.Scan() {
			line := s.Text()
			if m := declRE.FindStringSubmatch(line); m != nil {
				out[m[1]] = true
				continue
			}
			if m := ifaceMethodRE.FindStringSubmatch(line); m != nil {
				out[m[1]] = true
			}
		}
		if err := s.Err(); err != nil {
			return nil, fmt.Errorf("scan %s: %w", f, err)
		}
	}
	return out, nil
}

// tracked lists tracked files matching a pathspec. git ls-files rather than a
// filesystem walk, because a walk descends into gitignored .claude/worktrees/
// and would run a sibling branch's files against this tree.
func tracked(root, pathspec string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", pathspec) //nolint:gosec // G204: pathspec is a literal from this file's two call sites
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files %s: %w", pathspec, err)
	}
	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
