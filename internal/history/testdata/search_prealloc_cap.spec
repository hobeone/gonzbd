pkg ./internal/history/
run TestSearch_PreallocationCapBounded

[the preallocation clamp neutered]
file internal/history/repository.go
--- anchor
	if opts.Limit > maxPrealloc {
--- replace
	if false {
--- end
