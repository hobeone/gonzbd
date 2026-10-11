pkg ./internal/nzb/
run TestNoArticleProducedOutsideNZBPackage

[a production file outside internal/nzb constructs an Article]
file internal/app/ingest.go
--- anchor
func BuildIngestJob(cfg *config.Config, parsed *nzb.NZB,
--- replace
var _ = nzb.Article{ID: "forged@example.com", Bytes: 1, Number: 1}

func BuildIngestJob(cfg *config.Config, parsed *nzb.NZB,
--- end
