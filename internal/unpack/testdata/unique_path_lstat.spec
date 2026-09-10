pkg ./internal/unpack/
run TestUniquePath|TestNameIsFree|TestGoTar_ADanglingSymlink

# nameIsFree answers "is this name taken?", and Stat answers about a link's
# TARGET rather than the link. A symlink to a missing file therefore reads as
# absent, the undecorated name is handed back, and the extraction write follows
# the link to whatever it points at instead of landing on a fresh file.
#
# uniquePath's two call sites are NOT mutated separately any more: both go
# through nameIsFree, so there is one place the question is asked and one
# mutation that reverts it. That is the point of the helper -- the earlier shape
# had the same check written twice and needed a fixture per copy.

#
# Both mutations keep errors.Is in the replacement. Dropping it takes the last
# use of the "errors" import with it, and the mutated tree then fails to build
# -- which says nothing about whether the test discriminates. That is not
# hypothetical: it is what the first draft of both of these did.
[the existence probe follows the link]
file internal/unpack/unique_path.go
--- anchor
	_, err := root.Lstat(rel)
	return errors.Is(err, os.ErrNotExist)
--- replace
	_, err := root.Stat(rel)
	return errors.Is(err, os.ErrNotExist)
--- end

# The other half of nameIsFree: only ErrNotExist means free. Treating any error
# as absence hands back a name that could not be probed at all -- the shape the
# pre-refactor uniquePath had, and the reason it disagreed with
# fsutil.GetUniqueRelPath.
[any probe error is read as an unoccupied name]
file internal/unpack/unique_path.go
--- anchor
	_, err := root.Lstat(rel)
	return errors.Is(err, os.ErrNotExist)
--- replace
	_, err := root.Lstat(rel)
	return err != nil || errors.Is(err, os.ErrNotExist)
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
