pkg ./internal/postproc/
run ^(TestDeobfuscateStage_SkipsOnParError|TestDeobfuscateStage_SkipsOnUnpackError)$

[deobfuscate runs despite par error]
file internal/postproc/stage_deobfuscate.go
--- anchor
	case job.ParError:
--- replace
	case false:
--- end

[deobfuscate runs despite unpack error]
file internal/postproc/stage_deobfuscate.go
--- anchor
	case job.UnpackError:
--- replace
	case false:
--- end
