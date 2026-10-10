pkg ./internal/history/
run TestSearch_PreallocationCapBounded

[the preallocation clamp neutered]
file internal/history/repository.go
--- anchor
	if opts.Limit > maxPrealloc {
--- replace
	if false {
--- end

[the negative-limit guard widened to any nonzero limit]
file internal/history/repository.go
--- anchor
	} else if opts.Limit > 0 {
--- replace
	} else if opts.Limit != 0 {
--- end

[the requested page size ignored]
file internal/history/repository.go
--- anchor
	} else if opts.Limit > 0 {
--- replace
	} else if false {
--- end
