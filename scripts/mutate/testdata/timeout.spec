# Red check for the default go test deadline and how a timeout is classified
# (scripts/mutate/main.go): each mutation neuters one decision and a named test
# must die.
#
#     go run ./scripts/mutate scripts/mutate/testdata/timeout.spec
pkg ./scripts/mutate/
run TestTestArgs_|TestRunSpec_AMutationThatHangsIsKilled|TestRunSpec_ABaselineThatTimesOutIsABaselineFailure|TestTimedOut
timeout 5m

[a spec with no timeout line runs under go test's ten-minute default]
file scripts/mutate/main.go
--- anchor
	args = append(args, "-timeout", sp.testTimeout().String())
--- replace
	args = append(args, "-timeout", sp.timeout.String())
--- end

[a spec's own timeout is replaced by the default]
file scripts/mutate/main.go
--- anchor
	if sp.timeout > 0 {
		return sp.timeout
	}
--- replace
	if false && sp.timeout > 0 {
		return sp.timeout
	}
--- end

[the deadline's panic is matched anywhere in a line]
file scripts/mutate/main.go
--- anchor
var timedOutRe = regexp.MustCompile(`(?m)^panic: test timed out after (\S+)$`)
--- replace
var timedOutRe = regexp.MustCompile(`(?m)panic: test timed out after (\S+)$`)
--- end

[a hung mutation's row does not say it hung]
file scripts/mutate/main.go
--- anchor
		if d, ok := timedOut(out); ok {
			return result{mutation: m, verdict: killed, evidence: "the mutated test never finished: go test timed out after " + d}
--- replace
		if d, ok := timedOut(out); ok && false {
			return result{mutation: m, verdict: killed, evidence: "the mutated test never finished: go test timed out after " + d}
--- end

[a timed-out baseline is not told apart from a failing one]
file scripts/mutate/main.go
--- anchor
			if d, ok := timedOut(out); ok {
				fmt.Fprintf(os.Stderr, "\nthe unmutated test ran out of time (-timeout %s). If it needs that\n"+
--- replace
			if d, ok := timedOut(out); ok && false {
				fmt.Fprintf(os.Stderr, "\nthe unmutated test ran out of time (-timeout %s). If it needs that\n"+
--- end
