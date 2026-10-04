pkg ./internal/dispatch/store/
run TestStore_

[Save drops the unwanted state]
file internal/dispatch/store/store.go
--- anchor
		p.RecoveryBytes, p.Par2Recovered, p.Header.Unwanted,
--- replace
		p.RecoveryBytes, p.Par2Recovered, 0,
--- end
