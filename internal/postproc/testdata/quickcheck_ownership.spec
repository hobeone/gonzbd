pkg ./internal/postproc/
run TestQuickCheckStage_RelocationKeepsOwnership|TestQuickCheckStage_VerifiesAgainstThePostRenameName

# markRenamed moves ownership from the old absolute path to the new one.
# Pointing both ends at the old path is the shape of the bug this fixed —
# ownership tracked, but at the path the file no longer occupies.
#
# Mutated at the destination rather than by deleting the call, because
# stage_quickcheck.go imports path/filepath for this statement alone: removing
# it drops the last use and the tree fails to build, which would say nothing
# about whether the test discriminates.
[ownership is moved to the path the file just left]
file internal/postproc/stage_quickcheck.go
--- anchor
				filepath.Join(job.DownloadDir, filepath.FromSlash(r.To)))
--- replace
				filepath.Join(job.DownloadDir, r.From))
--- end

# Verification must read the names this stage's own relocation produced.
# JobProgress.Filename still holds the pre-rename name and this stage has no
# writer to correct it (postproc.Job carries a *queue.Job snapshot), so
# skipping the in-memory remap matches an absent name against the par2 index
# and marks an intact job damaged.
#
# Emptied at the map build rather than at the lookup, which keeps r and
# renamedTo used and the tree building.
[verification ignores the renames this stage just made]
file internal/postproc/stage_quickcheck.go
--- anchor
	for _, r := range renames {
		renamedTo[r.From] = r.To
	}
--- replace
	for _, r := range renames {
		_ = r.From
	}
--- end
