pkg ./internal/durability/
run TestStore_AdmitWritesTheFileStructure$

[Admit writes article_count as a literal zero]
file internal/durability/progress.go
--- anchor
f.Subject, f.Bytes, f.ArticleCount, int(f.FetchPolicy)
--- replace
f.Subject, f.Bytes, 0, int(f.FetchPolicy)
--- end
