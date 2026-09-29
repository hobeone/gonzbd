pkg ./internal/par2/
run TestIdentify_RecordsEachEntrysSet|TestAssessment_CRCExcluding

# Every entry Identify reads carries its set; without it every entry reads as
# belonging to the same set, and quickcheck cannot judge the sets apart.
[entries are not tagged with their set]
file internal/par2/fsops.go
--- anchor
			descs[i].Set = set.Name
--- replace
			descs[i].Set = ""
--- end

[CRCExcluding keeps an excluded set's identified files]
file internal/par2/assess.go
--- anchor
		if !sets[f.Desc.Set] {
--- replace
		if true {
--- end

[CRCExcluding keeps an excluded set's unaccounted entries]
file internal/par2/assess.go
--- anchor
		if !sets[fd.Set] {
--- replace
		if true {
--- end

[CRCExcluding verifies against no assembled files]
file internal/par2/assess.go
--- anchor
	return verifyIdentified(kept, a.files, log)
--- replace
	return verifyIdentified(kept, nil, log)
--- end
