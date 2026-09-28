# The red check for #563: naming a citation's self-match on a count mismatch.
#
# Run after changing selfMatchNote or its line-window logic:
#
#     go run ./scripts/mutate scripts/check_citations/testdata/self_match.spec
#
# No `run` filter: each mutation must be caught somewhere in the package's
# full test suite, the same convention scripts/mutate/testdata/self.spec uses.
pkg ./scripts/check_citations/
timeout 5m

[a self-match's line window is not checked, so a real match beside the comment is misreported as the comment's own]
file scripts/check_citations/main.go
--- anchor
		lineno, err := strconv.Atoi(m[2])
		if err != nil || lineno < c.line || lineno > c.endLine {
			continue
		}
--- replace
		lineno, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		_ = lineno
--- end
