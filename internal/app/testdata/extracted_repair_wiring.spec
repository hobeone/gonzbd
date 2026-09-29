pkg ./internal/app/
run TestBuildStages_ExtractedRepairUsesThePipelinesRepairStage|TestBuildStages_StageOrder

# A second repair stage of its own would not see a runtime change to the
# repair settings.
[extracted_repair gets a repair stage of its own]
file internal/app/stages.go
--- anchor
	extractedRepairStage := postproc.NewExtractedRepairStage(repairStage)
--- replace
	extractedRepairStage := postproc.NewExtractedRepairStage(postproc.NewRepairStage())
--- end

# Not in the pipeline, the deferred sets are never verified after unpack.
[extracted_repair is not in the pipeline]
file internal/app/stages.go
--- anchor
	stages = append(stages, extractedRepairStage)
--- replace
	_ = extractedRepairStage
--- end
