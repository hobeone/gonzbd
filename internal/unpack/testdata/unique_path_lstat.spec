pkg ./internal/unpack/
run TestUniquePath

# uniquePath answers "is this name taken?", and Stat answers about a link's
# TARGET rather than the link. A symlink to a missing file therefore reads as
# absent, the undecorated name is handed back, and the extraction write follows
# the link to whatever it points at instead of landing on a fresh file.
#
# The two sites are mutated separately because the first check returns before
# the candidate loop is ever reached, so pinning one says nothing about the
# other.

[the initial existence check follows the link]
file internal/unpack/unique_path.go
--- anchor
	if _, err := os.Lstat(destPath); err != nil {
--- replace
	if _, err := os.Stat(destPath); err != nil {
--- end

[the candidate loop's existence check follows the link]
file internal/unpack/unique_path.go
--- anchor
		if _, err := os.Lstat(candidate); err != nil {
--- replace
		if _, err := os.Stat(candidate); err != nil {
--- end
