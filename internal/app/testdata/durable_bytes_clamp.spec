pkg ./internal/app/
run TestDurableBytesOf_ClampsNegativeValues

[durable bytes does not clamp negative values]
file internal/app/statusinfo.go
--- anchor
	if durable < 0 {
		return 0
	}
--- replace
	if false {
		return 0
	}
--- end
