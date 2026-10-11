pkg ./internal/dispatch/
run TestRemove_PeerDeregistrationDuringRemoveIsSuccess

[Remove reports Cancel's not-found after a peer deregistered the job]
file internal/dispatch/registry.go
--- anchor
		if !errors.Is(err, ErrNotFound) {
--- replace
		if true {
--- end
