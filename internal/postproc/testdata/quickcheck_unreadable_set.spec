pkg ./internal/postproc/
run ^TestQuickCheckStage_UnreadableSetBeside

# A par2 set that read as nothing, with a held recovery volume of its own,
# keeps QuickCheck from a Clean or Unidentified verdict, so repair runs and
# its failure fetches the volume. A set with none leaves the verdict alone
# (#1008).

[an unreadable set is ignored]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(fetchable) > 0:
--- replace
	case false:
--- end

[a deferred set outranks an unreadable one]
file internal/postproc/stage_quickcheck.go
--- anchor
	case len(fetchable) > 0:
--- replace
	case len(fetchable) > 0 && !(len(deferred) > 0 && len(deferred) == len(setsWithEntries(a.ID))):
--- end

[an unreadable set with no held volume is sent to repair]
file internal/postproc/stage_quickcheck.go
--- anchor
func(name string) bool { return !held[name] })
--- replace
func(name string) bool { return held == nil && false })
--- end

[another set's held volume counts]
file internal/postproc/stage_quickcheck.go
--- anchor
func(name string) bool { return !held[name] })
--- replace
func(name string) bool { return len(held) == 0 })
--- end
