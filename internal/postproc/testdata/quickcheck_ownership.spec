pkg ./internal/postproc/
run TestQuickCheckStage_RelocationKeepsOwnership|TestQuickCheckStage_VerifiesBeforeRenaming

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

# THE ORDERING, which is what #494 is about.
#
# Relocating before assessing restores the sequence every defect came from: the
# moves land on disk, and the verdict is then computed against names that no
# longer describe anything. Under the old code this needed a local rename map
# to paper over; here it must simply be caught.
#
# Expressed by applying the renames FIRST and re-assessing after, which is the
# most faithful form of the old shape that still compiles.
[the verdict is taken after the renames are applied]
file internal/postproc/stage_quickcheck.go
--- anchor
	renames := par2.ApplyRenames(job.DownloadDir, a, log)
--- replace
	renames := par2.ApplyRenames(job.DownloadDir, a, log)
	a, _ = q.assess(job, sets, log)
--- end
