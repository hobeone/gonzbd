pkg ./internal/unpack/
run TestUniquePath|TestGoTar_ADanglingSymlink

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

# A third site in this package, and this one is not about picking a unique
# name: writeEntrySafely's OverwriteFiles=false skip check. Same
# Stat-answers-about-the-target bug, different consequence -- the entry silently
# replaces an existing dangling symlink instead of being skipped, which is not
# what the flag promises.
[the overwrite skip check follows the link]
file internal/unpack/write_entry.go
--- anchor
		if _, statErr := root.Lstat(destRel); statErr == nil {
--- replace
		if _, statErr := root.Stat(destRel); statErr == nil {
--- end
