pkg ./internal/assembler/
run TestDispatchRequest_ReleasesEveryArticleRequestOnce|TestDispatchRequest_ADuplicateIsReleasedOnItsOwn

[the worker never calls an article request's Release]
file internal/assembler/assembler.go
--- anchor
		defer req.Release()
--- replace
		defer func() {}()
--- end
