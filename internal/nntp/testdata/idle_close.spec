pkg ./internal/nntp/
run ^TestConn_Closed$|^TestRunReader_IdleDisconnectClassified$|^TestFinishReaderPrePop_Direct$|^TestIsServerIdleDisconnect$|^TestRunReader_UnsolicitedTeardownWakesPendingWithIdleClosed$|^TestSubmit_WriteOrFlushErrorWhenIdleClosedReturnsCloseError$

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

[finishReaderWithMode does not check len(orphans) == 0]
file internal/nntp/pipeline.go
--- anchor
		if prePop && len(orphans) == 0 && err != nil && !errors.Is(err, ErrIdleClosed) {
			err = fmt.Errorf("%w: %w", ErrIdleClosed, err)
		}
--- replace
		if prePop && err != nil && !errors.Is(err, ErrIdleClosed) {
			err = fmt.Errorf("%w: %w", ErrIdleClosed, err)
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

[runReader does not check isServerIdleDisconnect]
file internal/nntp/pipeline.go
--- anchor
		if isServerIdleDisconnect(code, text) {
			c.finishReader(fmt.Errorf("%w: nntp: unsolicited response %d %s", ErrIdleClosed, code, text))
			return
		}
--- replace
		if false && isServerIdleDisconnect(code, text) {
			c.finishReader(fmt.Errorf("%w: nntp: unsolicited response %d %s", ErrIdleClosed, code, text))
			return
		}
--- end

[runReader does not wrap ErrIdleClosed when pc == nil]
file internal/nntp/pipeline.go
--- anchor
		pc := c.popPending()
		if pc == nil {
			c.finishReader(fmt.Errorf("%w: nntp: unsolicited response %d %s", ErrIdleClosed, code, text))
			return
		}
--- replace
		pc := c.popPending()
		if pc == nil {
			c.finishReader(fmt.Errorf("nntp: unsolicited response %d %s", code, text))
			return
		}
--- end

[submit does not return closeError when write fails on idle closed connection]
file internal/nntp/pipeline.go
--- anchor
	if _, err := c.bw.Write(cmd); err != nil {
		c.unappendPending(pc)
		c.releaseSem()
		if closeErr := c.closeError(); errors.Is(closeErr, ErrIdleClosed) {
			return closeErr
		}
		return fmt.Errorf("nntp: write: %w", err)
	}
--- replace
	if _, err := c.bw.Write(cmd); err != nil {
		c.unappendPending(pc)
		c.releaseSem()
		if false {
			return nil
		}
		return fmt.Errorf("nntp: write: %w", err)
	}
--- end

[submit does not return closeError when flush fails on idle closed connection]
file internal/nntp/pipeline.go
--- anchor
	if err := c.bw.Flush(); err != nil {
		c.unappendPending(pc)
		c.releaseSem()
		if closeErr := c.closeError(); errors.Is(closeErr, ErrIdleClosed) {
			return closeErr
		}
		return fmt.Errorf("nntp: flush: %w", err)
	}
--- replace
	if err := c.bw.Flush(); err != nil {
		c.unappendPending(pc)
		c.releaseSem()
		if false {
			return nil
		}
		return fmt.Errorf("nntp: flush: %w", err)
	}
--- end
