pkg ./internal/unpack/
run TestGroupArchives_Single7zNotShadowedByPar2Backup

[sevenSplitPattern accepts 1-digit par2 backup suffix]
file internal/unpack/detect.go
--- anchor
var sevenSplitPattern = regexp.MustCompile(`(?i)\.7z\.(\d{3,})$`)
--- replace
var sevenSplitPattern = regexp.MustCompile(`(?i)\.7z\.(\d+)$`)
--- end
