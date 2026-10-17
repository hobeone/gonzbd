pkg ./internal/nntp/
run ^TestCloseReturnsReaderReason$|^TestFinishReaderIsIdempotent$|^TestFailPopped$|^TestSubmit_WriteOrFlushErrorWhenIdleClosedReturnsCloseError$

[closeCause never reads the recorded reason]
file internal/nntp/pipeline.go
--- anchor
	if s := c.closed.Load(); s != nil {
		return s.err
	}
--- replace
	if s := c.closed.Load(); false && s != nil {
		return s.err
	}
--- end

[finishReaderWithMode stores the close without its reason]
file internal/nntp/pipeline.go
--- anchor
		c.closed.Store(&closeState{err: err})
--- replace
		c.closed.Store(&closeState{})
--- end

[Close drops the reader's reason]
file internal/nntp/conn.go
--- anchor
	<-c.readerDone
	return c.closeCause()
--- replace
	<-c.readerDone
	return nil
--- end
