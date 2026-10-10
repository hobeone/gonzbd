pkg ./internal/job/
run TestPlaceRows_KeepsOnlyRowsOfTheFile|TestSettleFileCRC_DerivesStoresAndReleases|TestInstallCompleteFile_FailsTheRestAndSettles|TestUntrustFile_ReturnsTheFileToOutstanding|TestMarkArticleWritten_MarksDoneAndKeepsTheRow

# Design item 2 of the loose-record cut-over: the resident written rows
# (JobProgress.written) exist only for the whole-file CRC. They enter at
# verification and on each write, and leave when the CRC is settled or the
# file is untrusted.

[a settled file keeps its rows resident]
file internal/job/verified.go
--- anchor
	p.files[fileIdx].AssembledCRC32 = crc
	delete(p.written, fileIdx)
--- replace
	p.files[fileIdx].AssembledCRC32 = crc
--- end

[an untrusted file keeps its rows resident]
file internal/job/verified.go
--- anchor
	fp.AssembledCRC32 = 0
	delete(p.written, fileIdx)
--- replace
	fp.AssembledCRC32 = 0
--- end

# Design item 3: placeRows is where a row that cannot be placed is counted
# rather than installed.
[placeRows keeps a row with a negative offset]
file internal/job/verified.go
--- anchor
		if r.FileIdx != fileIdx || !m.ArticleInFile(r.FileIdx, r.ArtIdx) || r.Offset < 0 || r.Length <= 0 {
--- replace
		if r.FileIdx != fileIdx || !m.ArticleInFile(r.FileIdx, r.ArtIdx) || r.Length <= 0 {
--- end

[a written article's row is not kept for the CRC]
file internal/job/verified.go
--- anchor
	p.upsertRows(row.FileIdx, []durability.WrittenRow{row})
	return nil
--- replace
	_ = row
	return nil
--- end
