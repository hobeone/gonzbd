pkg ./internal/postproc/
run TestBuildDownloadFileList_Par2Summary

# The new case that stops a verdict-bearing, still-held job from being
# reported as "verified clean". Neutering the condition to false collapses it
# into the old heldVols>0 fallthrough, which is exactly the mislabel task 6
# removes — so the "unknown" subtest must go red.
[the verdict-bearing hold falls through to the clean case]
file internal/postproc/filelist.go
--- anchor
	case heldVols > 0 && !p.Par2Recovered() && p.HasPar2Verdict():
--- replace
	case false:
--- end
