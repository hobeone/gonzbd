pkg ./internal/job/
run TestNewFileTable_DerivesOffsetsAndTotals

[recovery capacity counts every .par2 instead of only recovery volumes]
file internal/job/filetable.go
--- anchor
			isPar2Recovery: IsRecoveryVolume(s.Subject),
--- replace
			isPar2Recovery: isPar2File(s.Subject),
--- end

[the article prefix sum is off by one]
file internal/job/filetable.go
--- anchor
		next += s.ArticleCount
--- replace
		next += s.ArticleCount + 1
--- end

[recovery bytes keep only the last volume instead of summing]
file internal/job/filetable.go
--- anchor
			bytes += f.bytes
--- replace
			bytes = f.bytes
--- end

[recovery file count is pinned at one instead of counting]
file internal/job/filetable.go
--- anchor
			count++
--- replace
			count = 1
--- end
