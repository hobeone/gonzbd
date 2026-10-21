pkg ./internal/job/
run ^TestFetchedRecoveryBlocks$

[a held volume counts as fetched]
file internal/job/progress.go
--- anchor
		if !m.Files().IsPar2Recovery(fi) || f.Fetch != FetchAlways || !f.Complete || f.FailedBytes != 0 {
--- replace
		if !m.Files().IsPar2Recovery(fi) || false && f.Fetch != FetchAlways || !f.Complete || f.FailedBytes != 0 {
--- end

[an incomplete volume counts as fetched]
file internal/job/progress.go
--- anchor
		if !m.Files().IsPar2Recovery(fi) || f.Fetch != FetchAlways || !f.Complete || f.FailedBytes != 0 {
--- replace
		if !m.Files().IsPar2Recovery(fi) || f.Fetch != FetchAlways || false && !f.Complete || f.FailedBytes != 0 {
--- end

[a volume with a failed article counts as fetched]
file internal/job/progress.go
--- anchor
		if !m.Files().IsPar2Recovery(fi) || f.Fetch != FetchAlways || !f.Complete || f.FailedBytes != 0 {
--- replace
		if !m.Files().IsPar2Recovery(fi) || f.Fetch != FetchAlways || !f.Complete || false && f.FailedBytes != 0 {
--- end
