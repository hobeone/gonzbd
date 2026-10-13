pkg ./internal/postproc/
run ^TestPostProc_HelperMethods$

# The history line of a DirectUnpack failure names its classification.

[a disk-full DirectUnpack failure is not named]
file internal/postproc/postproc.go
--- anchor
			if failure.Kind != unpack.FailUnknown {
--- replace
			if false {
--- end
