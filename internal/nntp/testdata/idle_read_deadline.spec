pkg ./internal/nntp/
run TestConnSurvivesIdlePeriodLongerThanTimeoutBetweenCommands|TestConnArmsReadDeadlineWhenCommandSubmittedAfterIdle|TestConnWriteErrorRollsBackInFlightDeadline|TestIdleTimeoutReader_GatedOnInFlight

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
		deadline = time.Now().Add(r.timeout)
		err = r.setReadDeadline(deadline)
	}
--- replace
	if false && wasIdle {
		deadline = time.Now().Add(r.timeout)
		err = r.setReadDeadline(deadline)
	}
--- end

[endCmd underflow guard removed]
file internal/nntp/conn.go
--- anchor
	if r.inFlight <= 0 {
		underflow = true
	} else {
--- replace
	if false && r.inFlight <= 0 {
		underflow = true
	} else {
--- end

[endCmd does not decrement inFlight]
file internal/nntp/conn.go
--- anchor
	} else {
		r.inFlight--
		if r.inFlight == 0 && r.gateOnInFlight {
--- replace
	} else {
		if r.inFlight == 0 && r.gateOnInFlight {
--- end

[endCmd does not disarm deadline when last command completes]
file internal/nntp/conn.go
--- anchor
		if r.inFlight == 0 && r.gateOnInFlight {
			err = r.setReadDeadline(time.Time{})
		}
--- replace
		if false && r.inFlight == 0 && r.gateOnInFlight {
			err = r.setReadDeadline(time.Time{})
		}
--- end

[endCmd disarms deadline even before endHandshake]
file internal/nntp/conn.go
--- anchor
		if r.inFlight == 0 && r.gateOnInFlight {
			err = r.setReadDeadline(time.Time{})
		}
--- replace
		if r.inFlight == 0 {
			err = r.setReadDeadline(time.Time{})
		}
--- end

[idleTimeoutReader.Read always arms timeout even when inFlight is zero]
file internal/nntp/conn.go
--- anchor
	if r.gateOnInFlight && r.inFlight == 0 {
		err = r.setReadDeadline(time.Time{})
	} else {
--- replace
	if false && r.gateOnInFlight && r.inFlight == 0 {
		err = r.setReadDeadline(time.Time{})
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
	c.itr.beginCmd()
	c.pendingLock.Unlock()
--- replace
	c.pending = append(c.pending, pc)
	c.pendingLock.Unlock()
--- end

[unappendPending does not call endCmd on write rollback]
file internal/nntp/pipeline.go
--- anchor
	if removed {
		c.pending = c.pending[:n-1]
		c.itr.endCmd()
	}
--- replace
	if removed {
		c.pending = c.pending[:n-1]
	}
--- end
