pkg ./internal/par2/
run TestAssess_VerifiesAFileAPreviousRunRelocated

# The basename join, pinned by BEHAVIOUR rather than by the helper's own
# output.
#
# identify.spec also kills this mutation, but via TestJoinKey — which asserts
# what joinKey returns, so it cannot distinguish "the join is wrong" from "the
# helper was changed deliberately". This spec requires the consequence to be
# caught: a file a previous run relocated is identified as "Screens/shot.jpg"
# while the caller still knows it as "shot.jpg", so a full-path join finds no
# CRC and reports an intact file unverified. Every caller reads that as damage.
[the identification/CRC join uses the full path]
file internal/par2/assess.go
--- anchor
	return filepath.Base(filepath.ToSlash(name))
--- replace
	return filepath.ToSlash(name)
--- end
