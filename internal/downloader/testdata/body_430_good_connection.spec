pkg ./internal/downloader/
run TestFetchArticle_FetchCountsNNTPNoArticle$

[neuter srv.RecordGoodConnection on BODY 430 ErrNoArticle]
file internal/downloader/dispatch.go
--- anchor
			// The server definitively said no. Try-list entry is
			// retained so we won't retry here; connection is
			// healthy — reuse it.
			srv.RecordGoodConnection()
			telemetry.PipelineErrors.Add(telemetry.ErrClassNNTPNoArticle, 1)
--- replace
			// The server definitively said no. Try-list entry is
			// retained so we won't retry here; connection is
			// healthy — reuse it.
			telemetry.PipelineErrors.Add(telemetry.ErrClassNNTPNoArticle, 1)
--- end
