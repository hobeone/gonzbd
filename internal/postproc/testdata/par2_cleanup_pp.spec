pkg ./internal/postproc/
run TestPar2Cleanup_PP0Skipped_PPVerifyRuns|TestShouldSkipForPP

[par2_cleanup omitted from shouldSkipForPP PPVerify gate]
file internal/postproc/postproc.go
--- anchor
	case "quickcheck", "repair", "par2_cleanup":
		return pp < types.PPVerify
--- replace
	case "quickcheck", "repair":
		return pp < types.PPVerify
--- end

[shouldSkipForPP gates par2_cleanup at PPUnpack instead of PPVerify]
file internal/postproc/postproc.go
--- anchor
	case "quickcheck", "repair", "par2_cleanup":
		return pp < types.PPVerify
	case "unpack":
		return pp < types.PPUnpack
--- replace
	case "quickcheck", "repair":
		return pp < types.PPVerify
	case "unpack", "par2_cleanup":
		return pp < types.PPUnpack
--- end
