pkg ./internal/app/
run TestRestoreResolution_KeepsRunsWhenFailedArticleScanFails

[a partial failed_articles read is abandoned instead of applied]
file internal/app/residency.go
--- anchor
		r.log.Warn("residency: read failed_articles", "job", j.ID(), "err", err)
--- replace
		r.log.Warn("residency: read failed_articles", "job", j.ID(), "err", err)
		if err != nil {
			return
		}
--- end
