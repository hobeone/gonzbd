pkg ./internal/job/
run ^(TestDoneClearingPaths_LeaveNoRowWithoutItsBit|TestMarkArticleWritten_ARewriteAfterARetrySettlesTheCRC|TestClearDone_LeavesACloneItsRows|TestMarkArticleWritten_MarksDoneAndKeepsTheRow|TestSettleFileCRC_DerivesStoresAndReleases)$

# #797: a resident written row exists only for a Done article. clearDone is
# the one clearer of a Done bit and drops the row with it, which is what lets
# MarkArticleWritten append without searching when it sets the bit. Each
# clearing site is reverted on its own.

[ResetForRetry clears the bits inline and keeps the row]
file internal/job/content.go
--- anchor
				j.progress.clearDone(fi, i)
--- replace
				j.progress.done.Clear(i)
				j.progress.failed.Clear(i)
--- end

[resetForReload clears the bits inline and keeps the row]
file internal/job/progress.go
--- anchor
	p.files[fi].FailedBytes -= bytes
	p.clearDone(fi, i)
--- replace
	p.files[fi].FailedBytes -= bytes
	p.done.Clear(i)
	p.failed.Clear(i)
--- end

# UntrustFile, the third clearing site, releases the file's rows in bulk
# before markNotDone runs, so reverting markNotDone changes no row:
# bitset_writers.spec pins it, and written_lifecycle.spec the bulk release.

[clearDone keeps the row]
file internal/job/progress.go
--- anchor
	p.dropRow(fi, i)
--- replace
	_ = fi
--- end

[dropRow edits the stored slice in place]
file internal/job/verified.go
--- anchor
	p.written[fi] = slices.Delete(slices.Clone(rows), k, k+1)
--- replace
	p.written[fi] = slices.Delete(rows, k, k+1)
--- end

[the append is taken for an article that already has a row]
file internal/job/verified.go
--- anchor
	if p.markDone(m, int(row.ArtIdx)) {
--- replace
	if p.markDone(m, int(row.ArtIdx)) || true {
--- end

[the newly-Done article's row is not kept]
file internal/job/verified.go
--- anchor
	p.written[row.FileIdx] = append(p.written[row.FileIdx], row)
--- replace
	_ = row
--- end
