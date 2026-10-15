pkg ./internal/nntp/
run TestConnSurvivesIdlePeriodLongerThanTimeoutBetweenCommands|TestConnArmsReadDeadlineWhenCommandSubmittedAfterIdle|TestIdleTimeoutReader_GatedOnInFlight

[Dial does not switch idleTimeoutReader to in-flight gating after handshake]
file internal/nntp/conn.go
--- anchor
	if itr != nil {
		itr.endHandshake() // handshake done; steady-state reads arm deadlines only while a command is in flight
	}
--- replace
	if itr != nil {
		itr.handshakeCtx = nil
	}
--- end

[beginCmd does not arm read deadline when transitioning from idle]
file internal/nntp/conn.go
--- anchor
	if wasIdle {
		_ = r.nc.SetReadDeadline(time.Now().Add(r.timeout)) //nolint:errcheck // best-effort arm when first command enters flight
	}
--- replace
	if false && wasIdle {
		_ = r.nc.SetReadDeadline(time.Now().Add(r.timeout)) //nolint:errcheck // best-effort arm when first command enters flight
	}
--- end

[endCmd does not decrement inFlight]
file internal/nntp/conn.go
--- anchor
	if r.inFlight > 0 {
		r.inFlight--
--- replace
	if false && r.inFlight > 0 {
		r.inFlight--
--- end

[idleTimeoutReader.Read always arms timeout even when inFlight is zero]
file internal/nntp/conn.go
--- anchor
	if r.gateOnInFlight && r.inFlight == 0 {
		_ = r.nc.SetReadDeadline(time.Time{}) //nolint:errcheck // best-effort disarm while no command is in flight
	} else {
--- replace
	if false && r.gateOnInFlight && r.inFlight == 0 {
		_ = r.nc.SetReadDeadline(time.Time{}) //nolint:errcheck // best-effort disarm while no command is in flight
	} else {
--- end

[runReader does not call endCmd when command completes]
file internal/nntp/pipeline.go
--- anchor
		pc.result = res
		close(pc.done)
		c.itr.endCmd()
--- replace
		pc.result = res
		close(pc.done)
--- end

[submit does not call beginCmd when queuing a command]
file internal/nntp/pipeline.go
--- anchor
	c.pending = append(c.pending, pc)
	c.pendingLock.Unlock()
	c.itr.beginCmd()
--- replace
	c.pending = append(c.pending, pc)
	c.pendingLock.Unlock()
--- end
