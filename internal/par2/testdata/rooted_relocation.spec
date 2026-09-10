pkg ./internal/par2/
run TestRelocateFile|TestIdentify|TestAssess

# A par2 File Description packet's filename is poster-controlled and becomes a
# filesystem path, in two places: relocateFile writes to it, and pass 0 of
# identification stats it. Both are confined by os.Root rather than by a lexical
# check on the string, and each site is mutated on its own.

# The write side, reverted to the unconfined os.* form this replaced — which is
# what a regression here would actually look like, since the lexical check that
# used to stand in front of it is gone.
#
# MkdirAll and Rename are mutated TOGETHER rather than separately, and that is a
# finding rather than a shortcut: either one alone refuses an escaping name, so
# reverting one leaves the other to produce the same verdict and the mutation
# survives while proving nothing. They are not two guards to be pinned apart;
# they are one property — every write goes through the root — applied at both
# calls.
[the relocation writes are not confined to the root]
file internal/par2/fsops.go
--- anchor
		if err := root.MkdirAll(destDir, 0o750); err != nil {
			log.Warn("quickcheck: failed to create directory",
				"dir", destDir, "err", err)
			return false
		}
	}

	// Move the file.
	if err := root.Rename(flatName, destRel); err != nil {
--- replace
		if err := os.MkdirAll(filepath.Join(root.Name(), destDir), 0o750); err != nil {
			log.Warn("quickcheck: failed to create directory",
				"dir", destDir, "err", err)
			return false
		}
	}

	// Move the file.
	if err := os.Rename(filepath.Join(root.Name(), flatName), filepath.Join(root.Name(), destRel)); err != nil {
--- end

# relocateFile's source-side Lstat. Stat follows a symlink at the final
# component, so the link is judged on its TARGET's size and then moved -- as a
# link -- to the path par2 names. Pass 0 refuses a non-regular file there on the
# next assessment, so the entry reads unaccounted from then on.
#
# Unlike the MkdirAll/Rename pair below, this guard and the regular-file check
# ARE separable, but only because the two subtests were built to separate them:
# a symlink's own size is its target string's length, so with a recorded length
# present the size comparison refuses it regardless of either guard.
[relocateFile stats through a symlinked source]
file internal/par2/fsops.go
--- anchor
	info, err := root.Lstat(flatName)
--- replace
	info, err := root.Stat(flatName)
--- end

# The regular-file requirement on the same stat, isolated by the subtest that
# gives par2 no recorded length -- with a length present the size comparison
# masks it.
[relocateFile moves whatever is at the source name]
file internal/par2/fsops.go
--- anchor
	if !info.Mode().IsRegular() {
		log.Warn("quickcheck: source is not a regular file, skipping",
			"file", flatName, "mode", info.Mode())
		return false
	}
--- replace
--- end

# Pass 0's stat. Stat follows a symlink at the final component, so an entry
# would be reported accounted from a link pointing at another file -- and
# Accounted() is what the download path consults before deciding whether to
# fetch recovery volumes.
[pass 0 follows a symlink at the final component]
file internal/par2/identify.go
--- anchor
		info, sErr := pass0Root.Lstat(filepath.FromSlash(slashed))
		if sErr != nil || !info.Mode().IsRegular() {
--- replace
		info, sErr := pass0Root.Stat(filepath.FromSlash(slashed))
		if sErr != nil || info.IsDir() {
--- end

# The length check on pass 0. Being at the right path is not evidence the file
# is whole, and claiming a truncated one reports the entry accounted while
# relocateFile -- which does compare -- declines to move it.
[pass 0 ignores the recorded length]
file internal/par2/identify.go
--- anchor
		if fd.FileSize > 0 && uint64(info.Size()) != fd.FileSize { //nolint:gosec // size is non-negative
			continue
		}
		claimedEntry[ei] = true
--- replace
		claimedEntry[ei] = true
--- end
