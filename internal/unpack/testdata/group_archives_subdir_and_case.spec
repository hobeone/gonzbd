pkg ./internal/unpack/
run ^(TestGroupArchives_DistinctSubdirectoriesDoNotCollide|TestGroupArchives_PreservesSplitArchiveCase)$

[archiveGroupKey drops filepath.Dir]
file internal/unpack/detect.go
--- anchor
func archiveGroupKey(path, lowerStem string) string {
	return filepath.Join(filepath.Dir(path), lowerStem)
}
--- replace
func archiveGroupKey(path, lowerStem string) string {
	return lowerStem
}
--- end

[SplitArchive does not preserve case from mainBase]
file internal/unpack/detect.go
--- anchor
		mainBase := filepath.Base(sorted[0])
		suffix := numericSuffixPattern.FindString(mainBase)
		stem := mainBase[:len(mainBase)-len(suffix)]
--- replace
		mainBase := filepath.Base(sorted[0])
		suffix := numericSuffixPattern.FindString(mainBase)
		stem := strings.ToLower(mainBase[:len(mainBase)-len(suffix)])
--- end

[groupArchives omits MainFile tie breaker]
file internal/unpack/detect.go
--- anchor
	slices.SortFunc(archives, func(a, b Archive) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.MainFile, b.MainFile))
	})
--- replace
	slices.SortFunc(archives, func(a, b Archive) int {
		return cmp.Compare(a.Name, b.Name)
	})
--- end
