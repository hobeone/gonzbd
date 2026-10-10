pkg ./internal/job/
run ^(TestBitsetWriters_MatchTheEnumerationStatedInProse|TestDoneBitWriters_MatchTheEnumerationStatedInProse)$

# The writers of each Set and Clear on the done, failed and emitted bitsets
# are pinned by name (#380, #797).

[markNotDone clears the bit inline]
file internal/job/progress.go
--- anchor
	p.clearDone(fi, i)
	return true
}

// clearDone
--- replace
	p.done.Clear(i)
	return true
}

// clearDone
--- end

[ClearArticleEmitted's evicted branch stops clearing the bit itself]
file internal/job/content.go
--- anchor
		j.progress.emitted.Clear(artIdx)
		return nil
--- replace
		j.progress.clearEmitted(j.manifest, artIdx)
		return nil
--- end

[setFailedBits stops setting the failed bit]
file internal/job/progress.go
--- anchor
	p.done.Set(i)
	p.failed.Set(i)
	p.emitted.Clear(i)
	return true
--- replace
	p.done.Set(i)
	p.emitted.Clear(i)
	return true
--- end
