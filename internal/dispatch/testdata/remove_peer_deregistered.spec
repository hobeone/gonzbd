pkg ./internal/dispatch/
run TestRemove_PeerDeregistrationDuringRemoveIsSuccess

[Remove reports Cancel's not-found even when a peer deregistered the job]
file internal/dispatch/registry.go
--- anchor
		if !errors.Is(err, ErrNotFound) || !rm.deregisteredByPeer() {
--- replace
		if true {
--- end

[the token never observes a peer deregistration]
file internal/dispatch/registry.go
--- anchor
	return r.d.byID[r.id] == nil
--- replace
	return false
--- end
