pkg ./internal/fsutil/
run TestGetUniqueRelPath_AllCases

# GetUniqueRelPath answers "is this name taken?", and Stat answers about a
# link's TARGET rather than the link. A symlink to a missing file therefore
# reads as absent, the undecorated name is handed back, and the caller's write
# follows the link. os.Root keeps that inside the root; it does not keep it off
# a file the caller never meant to touch.
#
# The two sites are mutated separately because the first check returns before
# the loop is ever reached, so pinning one says nothing about the other.

[the initial existence check follows the link]
file internal/fsutil/rootedcreate.go
--- anchor
	if _, err := root.Lstat(rel); err != nil {
--- replace
	if _, err := root.Stat(rel); err != nil {
--- end

[the suffix loop's existence check follows the link]
file internal/fsutil/rootedcreate.go
--- anchor
		if _, err := root.Lstat(newRel); err != nil {
--- replace
		if _, err := root.Stat(newRel); err != nil {
--- end
