pkg ./internal/postproc/
run TestBuildDownloadFileList_Par2Summary

# The case that stops a verdict-bearing, still-held job from being reported as
# "verified clean". Neutering the condition to false collapses it into the
# bare held-volumes fallthrough, which is exactly the mislabel it removes — so
# the "unknown" subtest must go red.
[the verdict-bearing hold falls through to the clean case]
file internal/postproc/filelist.go
--- anchor
	case heldVols > 0 && p.HasPar2Verdict():
--- replace
	case false:
--- end

# The fetched arm must fire for a partially released job, one volume released
# and one still held. Requiring every volume released sends it to the
# could-not-verify arm instead — the "recovered" subtest must go red.
[the fetched arm requires every volume released]
file internal/postproc/filelist.go
--- anchor
	case fetchedVols > 0:
--- replace
	case fetchedVols > 0 && heldVols == 0:
--- end

# Neutering the fetched arm collapses partially released jobs into the
# could-not-verify arm, which is the mislabel #505 removes.
[the fetched arm is neutered]
file internal/postproc/filelist.go
--- anchor
	case fetchedVols > 0:
--- replace
	case false:
--- end
