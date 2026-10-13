pkg ./internal/postproc/
run ^TestRepairStage_ARarSetDirectUnpackDidNotSettleDoesNotSkip$

# Repair is skipped on DirectUnpack's say only when every RAR set in the
# download directory is one DirectUnpack extracted, or a file it extracted.

[a RAR set DirectUnpack has no outcome for still counts as clean]
file internal/postproc/stage_repair.go
--- anchor
		if !covered[filepath.Base(a.MainFile)] && !covered[filepath.Clean(a.MainFile)] {
--- replace
		if false {
--- end

[a file a set extracted is not recognised]
file internal/postproc/stage_repair.go
--- anchor
			covered[filepath.Clean(f)] = true
--- replace
			_ = f
--- end
