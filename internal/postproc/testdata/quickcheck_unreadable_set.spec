pkg ./internal/postproc/
run ^TestQuickCheckStage_UnreadableSetBeside

# A par2 set that read as nothing keeps QuickCheck from a Clean or
# Unidentified verdict, so repair runs and its failure fetches the held
# recovery volumes (#1008).

[an unreadable set is ignored]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(a.ID.Unreadable) > 0:
--- replace
	case false:
--- end

[a deferred set outranks an unreadable one]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(a.ID.Unreadable) > 0:
--- replace
	case len(a.ID.Unreadable) > 0 && !(len(deferred) > 0 && len(deferred) == len(setsWithEntries(a.ID))):
--- end
