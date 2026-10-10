pkg ./internal/app/
run TestVerifyJobFiles_AZeroLengthRowDoesNotMoveTheTrimBound$

# A zero-length row claims no range, so it does not move the bound a file
# finished by path is truncated to.

[a zero-length row counts toward the trim bound]
file internal/app/verify.go
--- anchor
		if r.Length > 0 { // a zero-length article claims no range
--- replace
		if r.Length >= 0 { // a zero-length article claims no range
--- end
