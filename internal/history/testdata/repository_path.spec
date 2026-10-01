pkg ./internal/history/
run TestRepository_Path

[Repository.Path returns empty instead of forwarding the wrapped DB's path]
file internal/history/repository.go
--- anchor
func (r *Repository) Path() string { return r.path }
--- replace
func (r *Repository) Path() string { return "" }
--- end
