# The red check for #562: check_citations reading .sql comments.
#
# Run after changing SQL discovery or comment recognition:
#
#     go run ./scripts/mutate scripts/check_citations/testdata/sql_discovery.spec
#
# No `run` filter: each mutation must be caught somewhere in the package's
# full test suite, the same convention scripts/mutate/testdata/self.spec uses.
pkg ./scripts/check_citations/
timeout 5m

[SQL comments are not recognized as citations]
file scripts/check_citations/main.go
--- anchor
	if filepath.Ext(file) == ".sql" {
--- replace
	if false && filepath.Ext(file) == ".sql" {
--- end

[git ls-files drops *.sql again, so citableFiles reverts to Go only]
file scripts/check_citations/main.go
--- anchor
	cmd := exec.Command("git", "ls-files", "*.go", "*.sql")
--- replace
	cmd := exec.Command("git", "ls-files", "*.go")
--- end
