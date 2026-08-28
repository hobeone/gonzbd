package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtract_RejoinsACommandWrappedMidToken is the case the corpus actually
// produces and the one a naive per-line scanner gets wrong. Comment wrapping
// splits `grep -n` from its pattern, and in internal/sched/advance.go it splits
// `grep -v` from `_test.go` — so the two halves are on different lines and
// neither is a runnable command on its own.
func TestExtract_RejoinsACommandWrappedMidToken(t *testing.T) {
	src := "package p\n" +
		"// Acquisition happens ONLY here: `grep -n 'leases\\.issue'\n" +
		"// internal/sched/*.go | grep -v\n" +
		"// _test.go` finds exactly one line.\n" +
		"func f() {}\n"

	got := extract("x.go", src)
	if len(got) != 1 {
		t.Fatalf("extract found %d citations, want 1: %+v", len(got), got)
	}
	want := `grep -n 'leases\.issue' internal/sched/*.go | grep -v _test.go`
	if got[0].cmd != want {
		t.Errorf("cmd = %q, want %q", got[0].cmd, want)
	}
	if !got[0].hasWant || got[0].want != 1 {
		t.Errorf("count = (%d, %v), want (1, true)", got[0].want, got[0].hasWant)
	}
}

// TestExtract_IgnoresBackticksThatAreNotCommands pins that the tool reads only
// citations. This repository backticks identifiers and whole signatures in
// prose constantly — `func (q *Queue) finishCancel(j *job.Job, s job.Snapshot)`
// appears in internal/job/job.go — and treating one as a command would either
// refuse it noisily or, worse, try to run it.
func TestExtract_IgnoresBackticksThatAreNotCommands(t *testing.T) {
	src := "package p\n" +
		"// The signature is `func (q *Queue) finishCancel(j *job.Job)` and the\n" +
		"// field is `q.slots`, neither of which is a citation.\n"

	if got := extract("x.go", src); len(got) != 0 {
		t.Errorf("extract found %d citations in prose backticks, want 0: %+v", len(got), got)
	}
}

// TestExtract_SeparateBlocksDoNotBleed pins that two comment blocks separated
// by code are not joined. Joining them would let a count from one declaration's
// prose satisfy a command in another's, which is precisely the wrong-referent
// failure this tool exists to catch.
func TestExtract_SeparateBlocksDoNotBleed(t *testing.T) {
	src := "package p\n" +
		"// First: `grep -n 'alpha' a.go`\n" +
		"func a() {}\n" +
		"\n" +
		"// Second returns three.\n" +
		"func b() {}\n"

	got := extract("x.go", src)
	if len(got) != 1 {
		t.Fatalf("extract found %d citations, want 1", len(got))
	}
	if got[0].hasWant {
		t.Errorf("count = %d from a different block's prose, want none", got[0].want)
	}
}

func TestStatedCount(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  int
		ok    bool
	}{
		{"after, word", "x `grep -n 'a' b.go` finds four production lockers", 4, true},
		{"after, digit", "x `grep -n 'a' b.go` finds 7 call sites", 7, true},
		{"after, exactly", "x `grep -n 'a' b.go` finds exactly one line", 1, true},
		{"after, now exactly", "x `grep -n 'a' b.go` now finds exactly two lines", 2, true},
		{"nothing is zero", "x `grep -n 'a' b.go` returns nothing, so it is neither", 0, true},
		// job.go's shape: the count precedes the command.
		{"before", "There are five now — `grep -n 'a' b.go` inside Grant lists", 5, true},
		// The corpus names results instead of counting them; that must not be
		// guessed at, because a wrong guess reports a true comment as false.
		{"names results", "x `grep -n 'a' b.go` returns CancelJob and ForgetJob", 0, false},
		{"no count at all", "x `grep -n 'a' b.go` is the enumeration", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extract("x.go", "// "+tc.block+"\n")
			if len(got) != 1 {
				t.Fatalf("extract found %d citations, want 1", len(got))
			}
			if got[0].hasWant != tc.ok || (tc.ok && got[0].want != tc.want) {
				t.Errorf("count = (%d, %v), want (%d, %v)", got[0].want, got[0].hasWant, tc.want, tc.ok)
			}
		})
	}
}

// TestParsePipeline_RefusesWhatAShellWouldInterpret is the security pin. These
// strings come from source comments, so anything this tool is willing to run is
// something a comment can make it do. Each case below is a shape that would
// execute something other than grep if the command were handed to sh.
func TestParsePipeline_RefusesWhatAShellWouldInterpret(t *testing.T) {
	refuse := []string{
		`grep -n 'a' b.go; rm -rf /`,
		`grep -n 'a' b.go && curl evil.example`,
		`grep -n 'a' $(whoami).go`,
		`grep -n 'a' b.go > /etc/passwd`,
		`grep -n 'a' b.go | sh`,
		`grep -n 'a' b.go | xargs rm`,
		`sh -c 'grep -n a b.go'`,
		`grep -n "a" b.go`,
		`grep -n 'unbalanced b.go`,
	}
	for _, cmd := range refuse {
		t.Run(cmd, func(t *testing.T) {
			if _, err := parsePipeline(cmd); err == nil {
				t.Errorf("parsePipeline(%q) = nil error; this tool must never run it", cmd)
			}
		})
	}
}

func TestParsePipeline_AcceptsAGrepPipeline(t *testing.T) {
	stages, err := parsePipeline(`grep -n 'q\.mu\.Lock' internal/sched/*.go | grep -v _test.go`)
	if err != nil {
		t.Fatalf("parsePipeline: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("stages = %d, want 2", len(stages))
	}
	want := []string{"grep", "-n", `q\.mu\.Lock`, "internal/sched/*.go"}
	if strings.Join(stages[0].argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("stage 0 argv = %q, want %q", stages[0].argv, want)
	}
	if stages[0].pattern != `q\.mu\.Lock` {
		t.Errorf("pattern = %q, want the regex, not a flag or a path", stages[0].pattern)
	}
	if len(stages[0].paths) != 1 || stages[0].paths[0] != "internal/sched/*.go" {
		t.Errorf("paths = %q, want just the file operand", stages[0].paths)
	}
}

// TestParsePipeline_QuotedTextIsNotShellSyntax pins the distinction the first
// version missed: it scanned the raw command for metacharacters, so a regex
// anchor or a channel receive inside single quotes read as an injection
// attempt. A shell expands nothing inside '...', and neither should this.
func TestParsePipeline_QuotedTextIsNotShellSyntax(t *testing.T) {
	accept := []string{
		`grep -n 'StateUnset$' state.go`, // regex end anchor
		`grep -n '<-ctx.Done()' app.go`,  // channel receive
		`grep -nE 'foo|bar' x.go`,        // alternation, not a pipe
		`grep -n 'f = "s"' x.go`,         // double quote inside single quotes
		`grep -n 'a > b' x.go`,           // comparison
	}
	for _, cmd := range accept {
		t.Run(cmd, func(t *testing.T) {
			if _, err := parsePipeline(cmd); err != nil {
				t.Errorf("parsePipeline(%q) = %v; quoted text is literal", cmd, err)
			}
		})
	}
}

// TestParsePipeline_AcceptsGitGrep pins that the form AGENTS.md's Rule 4 uses
// in its own examples is understood. git grep is also the safer of the two:
// it resolves pathspecs against the repository and cannot be pointed outside
// it.
func TestParsePipeline_AcceptsGitGrep(t *testing.T) {
	stages, err := parsePipeline(`git grep -n 'X =' internal/job`)
	if err != nil {
		t.Fatalf("parsePipeline: %v", err)
	}
	if !stages[0].gitGrep {
		t.Error("gitGrep = false, want true")
	}
	if stages[0].pattern != "X =" {
		t.Errorf("pattern = %q, want %q", stages[0].pattern, "X =")
	}
}

// TestValidateOperands_KeepsGrepInsideTheRepository is the containment pin.
// Refusing shell metacharacters stops a citation running something else; it
// does nothing about a citation running grep exactly as intended against a file
// it should never see. On a mismatch this tool PRINTS the matching lines, so an
// unconstrained operand is an exfiltration primitive that needs no shell.
func TestValidateOperands_KeepsGrepInsideTheRepository(t *testing.T) {
	root := t.TempDir()
	cwd := root

	refuse := []string{
		`grep -a -n 'TOKEN' /proc/self/environ`,
		`grep -n 'x' ../../etc/passwd`,
		`grep -n 'x' /etc/passwd`,
		`grep -f /etc/passwd x.go`,
		`grep -n 'x'`, // no operand: reads os.DevNull, exits 1, looks like a clean zero
	}
	for _, cmd := range refuse {
		t.Run(cmd, func(t *testing.T) {
			stages, err := parsePipeline(cmd)
			if err != nil {
				return // refused earlier, which is also correct
			}
			if err := validateOperands(root, cwd, stages[0]); err == nil {
				t.Errorf("validateOperands(%q) = nil; this tool must never run it", cmd)
			}
		})
	}
}

// TestExpandArgs_NeverGlobsTheSearchPattern pins the corruption the first
// version shipped. isPath assumed argv[1] was the pattern, but a flag precedes
// it in nearly every citation here — so a regex containing *, ? or [ was
// globbed, and where it matched real filenames the pattern was REPLACED by
// them. grep then searched for the contents of a filename, silently.
func TestExpandArgs_NeverGlobsTheSearchPattern(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"ab", "aab", "target.go"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stages, err := parsePipeline("grep -n 'a*b' target.go")
	if err != nil {
		t.Fatalf("parsePipeline: %v", err)
	}
	got, err := expandArgs(dir, stages[0])
	if err != nil {
		t.Fatalf("expandArgs: %v", err)
	}
	if len(got) != 4 || got[2] != "a*b" {
		t.Errorf("argv = %q, want the pattern preserved and only operands globbed", got)
	}
}

// TestCitationDir_PicksTheDirectoryThePathIsWrittenAgainst pins the two
// conventions the corpus uses. A bare filename is written from the citing
// file's own directory — that is how it reads to someone with that file open —
// while a path with a separator is written from the repository root. Running
// either from the wrong directory yields zero matches, which looks exactly like
// a legitimate "returns nothing" answer.
func TestCitationDir_PicksTheDirectoryThePathIsWrittenAgainst(t *testing.T) {
	root := "/repo"
	c := citation{file: "internal/sched/advance.go"}

	mustStage := func(cmd string) stage {
		t.Helper()
		st, err := parsePipeline(cmd)
		if err != nil {
			t.Fatalf("parsePipeline(%q): %v", cmd, err)
		}
		return st[0]
	}

	bare := citationDir(root, c, mustStage("grep -n 'q.parkLocked(' advance.go"))
	if want := filepath.Join(root, "internal/sched"); bare != want {
		t.Errorf("bare filename → %q, want %q", bare, want)
	}

	if rooted := citationDir(root, c, mustStage("grep -n 'x' internal/sched/*.go")); rooted != root {
		t.Errorf("rooted path → %q, want %q", rooted, root)
	}

	// A slash in the PATTERN must not select the root. It did, which sent a
	// citation naming a file beside itself to a directory without that file.
	slashy := citationDir(root, c, mustStage("grep -n 'path/filepath' advance.go"))
	if want := filepath.Join(root, "internal/sched"); slashy != want {
		t.Errorf("pattern containing a slash → %q, want %q", slashy, want)
	}
}

// TestExpandArgs_LeavesANonMatchingGlobAlone pins the one case where being
// helpful would be wrong. If a glob matches nothing and this tool drops it from
// the argv, grep reads stdin and reports zero matches — indistinguishable from
// a citation that correctly states "returns nothing". Leaving the pattern in
// place makes grep fail loudly on a path that no longer exists.
func TestExpandArgs_LeavesANonMatchingGlobAlone(t *testing.T) {
	dir := t.TempDir()
	stages, err := parsePipeline("grep -n 'x' nosuch/*.go")
	if err != nil {
		t.Fatalf("parsePipeline: %v", err)
	}
	got, err := expandArgs(dir, stages[0])
	if err != nil {
		t.Fatalf("expandArgs: %v", err)
	}
	if got[len(got)-1] != "nosuch/*.go" {
		t.Errorf("argv = %q, want the unmatched glob preserved", got)
	}
}

func TestExpandArgs_ExpandsAMatchingGlob(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stages, err := parsePipeline("grep -n 'x' *.go")
	if err != nil {
		t.Fatalf("parsePipeline: %v", err)
	}
	got, err := expandArgs(dir, stages[0])
	if err != nil {
		t.Fatalf("expandArgs: %v", err)
	}
	if len(got) != 5 { // grep, -n, x, a.go, b.go
		t.Fatalf("argv = %q, want the glob expanded to two files", got)
	}
}

// TestRunCitation_CountsAndReportsMatches is the end-to-end check: a real file
// tree, a real grep, and a count compared against what the prose claimed.
func TestRunCitation_CountsAndReportsMatches(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "package pkg\nfunc a() { target() }\nfunc b() { target() }\n"
	if err := os.WriteFile(filepath.Join(sub, "x.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	c := citation{file: "pkg/x.go", cmd: "grep -n 'target()' pkg/*.go", want: 2, hasWant: true}
	got, lines, err := runCitation(dir, c)
	if err != nil {
		t.Fatalf("runCitation: %v", err)
	}
	if got != 2 {
		t.Errorf("count = %d, want 2 (lines: %q)", got, lines)
	}
	if len(lines) != 2 {
		t.Errorf("lines = %d, want 2: %q", len(lines), lines)
	}
}

// TestRunCitation_NoMatchIsZeroNotAnError pins that grep's exit status 1 is
// read as the answer it is. internal/job/state.go cites a command that
// correctly returns nothing; treating that as a failure would make a true
// citation unreportable.
func TestRunCitation_NoMatchIsZeroNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := citation{file: "x.go", cmd: "grep -n 'absent' x.go", want: 0, hasWant: true}
	got, _, err := runCitation(dir, c)
	if err != nil {
		t.Fatalf("runCitation on a no-match command: %v", err)
	}
	if got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
}

// TestRunCitation_MissingPathIsAnErrorNotZero pins the difference between "the
// command found nothing" and "the command could not run". internal/job/job.go
// carried `grep -n 'return Err' ` with no path argument at all: grep then reads
// stdin and reports zero, which reads as a legitimate answer. It is not one.
func TestRunCitation_MissingPathIsAnErrorNotZero(t *testing.T) {
	dir := t.TempDir()
	// The real shape: no path OPERAND at all. Go leaves a nil Stdin as
	// os.DevNull, so grep reads EOF and exits 1 — which runStages treats as an
	// honest zero. An earlier version of this test used a non-existent
	// filename instead, which fails for a different reason (exit 2) and left
	// the case its own comment named untested.
	c := citation{file: "x.go", cmd: "grep -n 'return Err'"}
	if n, _, err := runCitation(dir, c); err == nil {
		t.Errorf("runCitation with no operand = (%d, nil); want a failure, not a clean zero", n)
	}
}

// TestRunCitation_CountsBlankLineMatches pins TrimSuffix over TrimRight. grep
// ends every record with a newline, so exactly one is a delimiter; stripping
// all of them reduces the output of a blank-line search to "" and reports a
// real count as zero.
func TestRunCitation_CountsBlankLineMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := citation{file: "x.go", cmd: "grep -c '^$' x.txt"}
	got, _, err := runCitation(dir, c)
	if err != nil {
		t.Fatalf("runCitation: %v", err)
	}
	if got != 1 { // grep -c prints one line: the count
		t.Errorf("count = %d, want 1 output line from grep -c", got)
	}
}

// TestRunStages_CarriesGrepsStderr pins that a real grep failure explains
// itself. Without it the report says only "exit status 2", which tells the
// reader nothing about what grep objected to.
func TestRunStages_CarriesGrepsStderr(t *testing.T) {
	dir := t.TempDir()
	stages, err := parsePipeline("grep -n 'x' nosuchfile.go")
	if err != nil {
		t.Fatalf("parsePipeline: %v", err)
	}
	_, err = runStages(dir, stages)
	if err == nil {
		t.Fatal("runStages on a missing file = nil error")
	}
	if !strings.Contains(err.Error(), "nosuchfile.go") || !strings.Contains(strings.ToLower(err.Error()), "no such file") {
		t.Errorf("error = %q, want grep's own stderr included", err)
	}
}

// TestExtract_LineIsTheOpeningBacktick pins that a wrapped citation reports
// where it starts. Reporting the closing backtick sends the reader to the
// wrong line, which for a three-line command is the line after the claim.
func TestExtract_LineIsTheOpeningBacktick(t *testing.T) {
	src := "package p\n" +
		"// starts here `grep -n 'x'\n" +
		"// y.go` finds one\n"
	got := extract("f.go", src)
	if len(got) != 1 {
		t.Fatalf("extract found %d citations, want 1", len(got))
	}
	if got[0].line != 2 {
		t.Errorf("line = %d, want 2 (the // line carrying the opening backtick)", got[0].line)
	}
}

// TestSelfPkg_IsThisPackagesOwnDirectory pins the self-skip against the path
// it names. The skip is a string comparison, so moving or renaming this
// directory would silently stop skipping it — and the failure mode is not a
// crash but a report full of findings against comments that document the
// convention rather than use it, which reads as the tool working.
func TestSelfPkg_IsThisPackagesOwnDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.ToSlash(wd); !strings.HasSuffix(got, selfPkg) {
		t.Errorf("selfPkg = %q, but this test runs in %q; the skip no longer names this package", selfPkg, got)
	}
}

// TestStatedCount_BoundedByNeighbouringCitations pins the window rule against
// the two shapes that broke it, both drawn from the real corpus.
//
// internal/job/attempt.go carries two citations in ONE comment block, each with
// its own count. An unbounded window read the first's "returns exactly four
// lines" as the second's claim. Clamping at every backtick fixed that and broke
// the other shape: scenario_test.go explains its pattern with a backticked
// identifier immediately after the command, and clamping there cut the real
// count off before it was reached.
func TestStatedCount_BoundedByNeighbouringCitations(t *testing.T) {
	t.Run("a neighbouring citation's count is not borrowed", func(t *testing.T) {
		src := "// first: `grep -n 'a' x.go` returns exactly four lines, and\n" +
			"// second: `grep -n 'b' x.go` returns exactly those two lines.\n"
		got := extract("f.go", src)
		if len(got) != 2 {
			t.Fatalf("extract found %d citations, want 2", len(got))
		}
		if !got[0].hasWant || got[0].want != 4 {
			t.Errorf("first = (%d, %v), want (4, true)", got[0].want, got[0].hasWant)
		}
		if !got[1].hasWant || got[1].want != 2 {
			t.Errorf("second = (%d, %v), want (2, true) — not the first citation's four",
				got[1].want, got[1].hasWant)
		}
	})

	t.Run("a backticked identifier in the prose is not a boundary", func(t *testing.T) {
		src := "// `grep -n 'q\\.releaseFor(' internal/sched/*.go | grep -v _test.go`\n" +
			"// (the `q\\.` prefix excludes the func declaration) finds seven sites.\n"
		got := extract("f.go", src)
		if len(got) != 1 {
			t.Fatalf("extract found %d citations, want 1", len(got))
		}
		if !got[0].hasWant || got[0].want != 7 {
			t.Errorf("count = (%d, %v), want (7, true) — the inline backtick must not cut the window",
				got[0].want, got[0].hasWant)
		}
	})
}

// TestStatedCount_BareNoIsNotACount pins that a determiner is not a number.
// internal/job/attempt.go says "started and ended have no reader yet", which an
// earlier number-word table read as the count zero and reported as a wrong
// citation. "nothing" and "zero" stay, because both only ever appear as counts.
func TestStatedCount_BareNoIsNotACount(t *testing.T) {
	got := extract("f.go", "// started and ended have no reader yet — `grep -n 'x' x.go` lists them.\n")
	if len(got) != 1 {
		t.Fatalf("extract found %d citations, want 1", len(got))
	}
	if got[0].hasWant {
		t.Errorf("count = %d parsed from \"have no reader\", want none", got[0].want)
	}
}
