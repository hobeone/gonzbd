pkg ./internal/job/
run Test(InstallFileVerification_CompleteFileKeepsAZeroLengthRow|InstallVerified_KeepsAZeroLengthRow|PlaceRows_KeepsOnlyRowsOfTheFile)$

# A zero-length row MarkArticleWritten accepts live is placed after a restart
# too.

[placeRows drops a zero-length row as it once did]
file internal/job/verified.go
--- anchor
|| !r.HasValidShape() {
--- replace
|| !r.HasValidShape() || r.Length == 0 {
--- end

[placeRows places a row whatever its shape]
file internal/job/verified.go
--- anchor
|| !r.HasValidShape() {
--- replace
|| false {
--- end
