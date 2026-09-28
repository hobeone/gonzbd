pkg ./internal/app/
run TestBuildStages_QuickCheckSeesThePipelinesUnpackStage

# Unwired, quickcheck can never see that unpack will run, so it never records
# Unidentified and every Layout B post fails its repair again.
[quickcheck is not given the unpack stage]
file internal/app/stages.go
--- anchor
	qcStage.Unpack = unpackStage
--- replace
--- end
