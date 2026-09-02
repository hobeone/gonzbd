pkg ./internal/postproc/
run TestQuickCheckStage_RelocationKeepsOwnership

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
