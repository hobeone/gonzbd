# Red check for the default go test deadline and how a timeout is classified
# (scripts/mutate/main.go): each mutation neuters one decision and a named test
# must die. The default's own arms are in timeout_args.spec, whose run line
# leaves out the tests here that wait for a deadline: under a mutation that
# lengthens the deadline they would wait it out.
#
#     go run ./scripts/mutate scripts/mutate/testdata/timeout.spec
pkg ./scripts/mutate/
run TestRunSpec_AMutationThatHangsIsKilled|TestRunSpec_ABaselineThatTimesOutIsABaselineFailure|TestTimedOut
timeout 5m

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
