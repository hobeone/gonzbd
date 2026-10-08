pkg ./internal/assembler/
run Test(PreallocateLinuxFallback|PreallocateFileZeroSize)$

[EOPNOTSUPP arm neutered, returns error instead of nil]
file internal/assembler/preallocate_linux.go
--- anchor
	if err == nil || errors.Is(err, unix.EOPNOTSUPP) {
--- replace
	if err == nil || errors.Is(err, unix.EOPNOTSUPP) && false {
--- end

[size <= 0 boundary shifted to size < 0]
file internal/assembler/preallocate_linux.go
--- anchor
	if size <= 0 {
--- replace
	if size < 0 {
--- end
