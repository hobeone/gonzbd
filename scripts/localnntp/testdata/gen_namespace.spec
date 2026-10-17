pkg ./scripts/localnntp/
run TestGenerateRelease_MessageIDNamespace

[the namespace ignores the release name]
file scripts/localnntp/gen.go
--- anchor
	return fmt.Sprintf("%s/%d", name, seed)
--- replace
	return fmt.Sprintf("%d", seed)
--- end

[the namespace ignores the seed]
file scripts/localnntp/gen.go
--- anchor
	return fmt.Sprintf("%s/%d", name, seed)
--- replace
	return fmt.Sprintf("%s", name)
--- end

[GenerateRelease passes a constant namespace]
file scripts/localnntp/gen.go
--- anchor
SliceFileSegments(releaseIDNamespace(cfg.Name, cfg.Seed), filePath,
--- replace
SliceFileSegments("fixed", filePath,
--- end
