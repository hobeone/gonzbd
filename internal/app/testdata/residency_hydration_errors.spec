pkg ./internal/app/
run TestRestoreResolution_KeepsRunsWhenFailedArticleScanFails

[the failed_articles scan error returns instead of breaking]
file internal/app/residency.go
--- anchor
			r.log.Warn("residency: scan failed_articles", "job", j.ID(), "err", err)
			break
--- replace
			r.log.Warn("residency: scan failed_articles", "job", j.ID(), "err", err)
			return
--- end
