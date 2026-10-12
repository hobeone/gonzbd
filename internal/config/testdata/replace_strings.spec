pkg ./internal/config/
run ^TestReplaceStrings_RefusedOnSetAndLoad$

# Both replacement strings are refused when they hold a separator or NUL,
# through Set and Load alike (both run Validate).

[replace_illegal_with is not checked]
file internal/config/validate.go
--- anchor
	if err := fsutil.CheckReplacement(d.ReplaceIllegalWith); err != nil {
--- replace
	if err := fsutil.CheckReplacement(d.ReplaceIllegalWith); false && err != nil {
--- end

[replace_spaces_with is not checked]
file internal/config/validate.go
--- anchor
	if err := fsutil.CheckReplacement(d.ReplaceSpacesWith); err != nil {
--- replace
	if err := fsutil.CheckReplacement(d.ReplaceSpacesWith); false && err != nil {
--- end
