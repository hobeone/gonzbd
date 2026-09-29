pkg ./internal/par2/
run TestGoRepair_MissingFileFoundElsewhere_DoesNotReportSuccess

[Missing-file downgrade neutered]
file internal/par2/go_par2.go
--- anchor
	if len(missing) == 0 {
--- replace
	if true {
--- end
