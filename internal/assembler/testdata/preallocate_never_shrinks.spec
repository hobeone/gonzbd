pkg ./internal/assembler/
run TestGrowFileNeverShrinks

[the grow-only guard dropped, ftruncate always runs]
file internal/assembler/preallocate.go
--- anchor
	if fi.Size() >= size {
--- replace
	if fi.Size() >= size && false {
--- end
