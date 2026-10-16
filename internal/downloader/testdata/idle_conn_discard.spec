pkg ./internal/downloader/
run ^TestFetchArticle_IdleClosedConnRedialsWithoutPenalty$|^TestPenaltyFor$

[managedConn.Get returns closed conn without discarding]
file internal/downloader/dispatch.go
--- anchor
	if m.conn != nil {
		if !m.conn.Closed() {
			c := m.conn
			m.mu.Unlock()
			if m.hookAfterGet != nil {
				m.hookAfterGet(c)
			}
			return c, nil
		}
		_ = m.conn.Close() //nolint:errcheck // discarding an already-closed conn
--- replace
	if m.conn != nil {
		if true {
			c := m.conn
			m.mu.Unlock()
			if m.hookAfterGet != nil {
				m.hookAfterGet(c)
			}
			return c, nil
		}
		_ = m.conn.Close() //nolint:errcheck // discarding an already-closed conn
--- end

[fetchArticle does not drop and unmark idle closed connection]
file internal/downloader/dispatch.go
--- anchor
	body, err := c.Fetch(fetchCtx, req.messageID)
	if err != nil {
		if errors.Is(err, nntp.ErrIdleClosed) {
			mc.DropIfMatches(c, d, workerID)
			d.unmarkTried(req, serverIdx)
			return nil, false
		}
--- replace
	body, err := c.Fetch(fetchCtx, req.messageID)
	if err != nil {
		if false && errors.Is(err, nntp.ErrIdleClosed) {
			mc.DropIfMatches(c, d, workerID)
			d.unmarkTried(req, serverIdx)
			return nil, false
		}
--- end

[PenaltyFor applies default penalty to ErrIdleClosed]
file internal/downloader/penalty.go
--- anchor
	case errors.Is(err, nntp.ErrIdleClosed):
		// Connection closed or received an unsolicited timeout while no
		// command was in flight — normal idle teardown, not a server fault.
		return 0
--- replace
	case false && errors.Is(err, nntp.ErrIdleClosed):
		// Connection closed or received an unsolicited timeout while no
		// command was in flight — normal idle teardown, not a server fault.
		return 0
--- end
