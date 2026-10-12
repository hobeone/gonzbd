pkg ./internal/fsutil/
run ^TestWriteAtomic_ChmodFlagGatesThePermission$

# writeAtomic applies perm to the published file exactly when chmod is set.

[perm is applied without the flag]
file internal/fsutil/atomic.go
--- anchor
	if chmod {
--- replace
	if true {
--- end

[perm is ignored with the flag]
file internal/fsutil/atomic.go
--- anchor
	if chmod {
--- replace
	if false {
--- end
