pkg ./internal/fsutil/
run ^(TestSanitize_ResultIsOneComponentWhateverTheReplacements|TestCheckReplacement)$

# The sanitizers' result is one path component whatever the replacement
# strings hold: a replacement CheckReplacement refuses is not used.

[an unsafe illegal-character replacement is used]
file internal/fsutil/sanitize.go
--- anchor
	if illegalReplacement == "" || CheckReplacement(illegalReplacement) != nil {
--- replace
	if illegalReplacement == "" {
--- end

[an unsafe space replacement is used]
file internal/fsutil/sanitize.go
--- anchor
	if opts.ReplaceSpacesWith != "" && CheckReplacement(opts.ReplaceSpacesWith) == nil {
--- replace
	if opts.ReplaceSpacesWith != "" {
--- end

[CheckReplacement admits anything]
file internal/fsutil/sanitize.go
--- anchor
	if i := strings.IndexAny(s, illegalChars); i >= 0 {
--- replace
	if i := strings.IndexAny(s, illegalChars); false && i >= 0 {
--- end
