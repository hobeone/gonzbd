pkg ./internal/nntp/
run ^TestConn_Closed$|^TestRunReader_IdleDisconnectClassified$|^TestFinishReaderPrePop_Direct$

[Conn.Closed returns false unconditionally]
file internal/nntp/conn.go
--- anchor
func (c *Conn) Closed() bool {
	return c.closed.Load()
}
--- replace
func (c *Conn) Closed() bool {
	return false
}
--- end

[finishReaderWithMode does not wrap idle disconnect with ErrIdleClosed]
file internal/nntp/pipeline.go
--- anchor
		if prePop && len(orphans) == 0 && err != nil && !errors.Is(err, ErrIdleClosed) {
			err = fmt.Errorf("%w: %w", ErrIdleClosed, err)
		}
--- replace
		if false && len(orphans) == 0 && err != nil && !errors.Is(err, ErrIdleClosed) {
			err = fmt.Errorf("%w: %w", ErrIdleClosed, err)
		}
--- end

[runReader calls finishReader instead of finishReaderPrePop on read error]
file internal/nntp/pipeline.go
--- anchor
		line, err := readResponseLine(c.br)
		if err != nil {
			c.finishReaderPrePop(err)
			return
		}
--- replace
		line, err := readResponseLine(c.br)
		if err != nil {
			c.finishReader(err)
			return
		}
--- end

[runReader calls finishReader instead of finishReaderPrePop on unsolicited response]
file internal/nntp/pipeline.go
--- anchor
		pc := c.popPending()
		if pc == nil {
			c.finishReaderPrePop(fmt.Errorf("nntp: unsolicited response %d %s", code, text))
			return
		}
--- replace
		pc := c.popPending()
		if pc == nil {
			c.finishReader(fmt.Errorf("nntp: unsolicited response %d %s", code, text))
			return
		}
--- end
