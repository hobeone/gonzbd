pkg ./internal/postproc/
run ^(TestRepairStage_QuickCheckDisabledRepairsBesideDirectUnpackSuccess|TestRepairStage_HandlesEveryQuickCheckOutcome)$

# Repair runs for every quickcheck outcome but Clean and Unidentified. A
# DirectUnpack success is not a reason to skip it (#953).

[the DirectUnpack shortcut restored for NotRun]
file internal/postproc/stage_repair.go
--- anchor
	case QuickCheckNotRun, QuickCheckDamaged, QuickCheckInconclusive:
--- replace
	case QuickCheckNotRun:
		if len(job.DirectUnpackSets) > 0 && len(job.DirectUnpackFailures) == 0 && len(job.DirectUnpackSkipped) == 0 {
			return nil
		}
	case QuickCheckDamaged, QuickCheckInconclusive:
--- end

[NotRun skips repair]
file internal/postproc/stage_repair.go
--- anchor
	case QuickCheckNotRun, QuickCheckDamaged, QuickCheckInconclusive:
--- replace
	case QuickCheckNotRun:
		return nil
	case QuickCheckDamaged, QuickCheckInconclusive:
--- end

[Damaged skips repair]
file internal/postproc/stage_repair.go
--- anchor
	case QuickCheckNotRun, QuickCheckDamaged, QuickCheckInconclusive:
--- replace
	case QuickCheckDamaged:
		return nil
	case QuickCheckNotRun, QuickCheckInconclusive:
--- end

[Inconclusive skips repair]
file internal/postproc/stage_repair.go
--- anchor
	case QuickCheckNotRun, QuickCheckDamaged, QuickCheckInconclusive:
--- replace
	case QuickCheckInconclusive:
		return nil
	case QuickCheckNotRun, QuickCheckDamaged:
--- end
