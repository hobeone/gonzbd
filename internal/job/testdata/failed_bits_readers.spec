pkg ./internal/job/
run ^(TestMarkArticleFailed_EvictedRejectsAnOutOfRangeIndex|TestInstallFileVerification_CompleteFileFailsTheRestAndSettles)$

# The failed-bit checks that replaced AnyArticleFailed: an out-of-range
# failure sets no article's bit, and a whole complete file fails none of its
# articles.

[an out-of-range evicted failure sets the nearest bit]
file internal/job/content.go
--- anchor
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.manifest == nil {
		j.progress.setFailedBits(artIdx)
--- replace
		if j.manifest == nil {
			j.progress.setFailedBits(min(max(artIdx, 0), j.progress.TotalArticles()-1))
		}
		return fmt.Errorf("job %s: artIdx %d out of range", j.id, artIdx)
	}
	if j.manifest == nil {
		j.progress.setFailedBits(artIdx)
--- end

[a complete file's written articles are failed too]
file internal/job/verification.go
--- anchor
		_ = p.markFailed(m, i) // a no-op for an article a row just marked Done
--- replace
		p.failed.Set(i)
--- end
