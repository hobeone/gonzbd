package nntp

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// cmdKind distinguishes what kind of response the caller expects so the
// reader can tell whether a dot-terminated multi-line body follows the
// status line. Error codes (4xx/5xx) never produce a body regardless
// of kind — the reader consults both kind and code.
type cmdKind uint8

const (
	cmdBody cmdKind = iota + 1
	cmdArticle
	cmdHead
	cmdStat
	cmdCapabilities
	cmdDate
	cmdQuit
	cmdAuthInfoUser
	cmdAuthInfoPass
)

// pendingCmd is a placeholder entry in the FIFO of outstanding
// commands. When the reader goroutine pops it off the FIFO, it fills
// result and closes done. Callers that cancel their context set
// orphaned=1; the reader still reads+discards the corresponding
// response so the connection stays in sync.
type pendingCmd struct {
	kind     cmdKind
	done     chan struct{} // closed by reader on completion
	result   cmdResult     // valid only after done is closed
	orphaned atomic.Bool
}

type cmdResult struct {
	code int
	line string // response line after the 3-digit code and space
	body []byte // populated for multi-line responses
	err  error  // non-nil on I/O or protocol errors
}

// expectedBodyCode reports the status code that marks a multi-line
// response for each command kind. Any other code means "single-line
// response, usually an error."
func (k cmdKind) expectedBodyCode() (int, bool) {
	switch k {
	case cmdBody:
		return 222, true
	case cmdArticle:
		return 220, true
	case cmdHead:
		return 221, true
	case cmdCapabilities:
		return 101, true
	}
	return 0, false
}

// runReader consumes responses from br in order and dispatches them to
// the head of the pending FIFO. It returns when the socket errors or
// closes; the returned error is recorded on the Conn and signalled to
// any still-waiting callers.
//
// The caller is expected to invoke runReader in its own goroutine. The
// method holds no locks for the duration of the read; pendingLock is
// taken only momentarily to pop the FIFO head.
func (c *Conn) runReader() {
	defer close(c.readerDone)

	for {
		// Idle timeout is enforced per-Read by idleTimeoutReader
		// wrapping nc, so no per-command SetReadDeadline is needed.
		line, err := readResponseLine(c.br)
		if err != nil {
			c.finishReader(err)
			return
		}
		code, text, perr := parseStatus(line)
		if perr != nil {
			c.finishReader(perr)
			return
		}

		pc := c.popPending()
		if pc == nil {
			c.finishReader(fmt.Errorf("nntp: unsolicited response %d %s", code, text))
			return
		}

		res := cmdResult{code: code, line: text}
		if expected, multi := pc.kind.expectedBodyCode(); multi && code == expected {
			body, berr := readDotStuffedBody(c.br)
			if berr != nil {
				res.err = berr
				pc.result = res
				close(pc.done)
				c.finishReader(berr)
				return
			}
			res.body = body
		}
		pc.result = res
		close(pc.done)

		// Pull the semaphore slot back now that the command is
		// complete. Orphaned commands still land here — the caller
		// already returned; this just lets the next command proceed.
		select {
		case <-c.sem:
		default:
			// Should never happen: every pendingCmd corresponds to a
			// semaphore acquire. Treat as fatal.
			c.finishReader(errors.New("nntp: semaphore underflow"))
			return
		}
	}
}

// submit appends pc to the pending FIFO and writes cmd to the wire,
// both under sendLock so the on-wire order exactly matches the FIFO
// order. The caller must have already acquired the pipelining
// semaphore; on error, submit releases it so the slot isn't leaked.
func (c *Conn) submit(pc *pendingCmd, cmd []byte) error {
	c.sendLock.Lock()
	defer c.sendLock.Unlock()

	// Check closed inside pendingLock to prevent a race with
	// finishReader: if finishReader sets closed=true and drains
	// pending, a concurrent submit must not re-append after the drain.
	c.pendingLock.Lock()
	if c.closed.Load() {
		c.pendingLock.Unlock()
		c.releaseSem()
		return c.closeError()
	}
	c.pending = append(c.pending, pc)
	c.pendingLock.Unlock()

	if _, err := c.bw.Write(cmd); err != nil {
		c.unappendPending(pc)
		c.releaseSem()
		return fmt.Errorf("nntp: write: %w", err)
	}
	if err := c.bw.Flush(); err != nil {
		c.unappendPending(pc)
		c.releaseSem()
		return fmt.Errorf("nntp: flush: %w", err)
	}
	return nil
}

// unappendPending removes pc from the pending FIFO, used when a
// write fails after the append. pc is expected to be the most recent
// entry; if it isn't, something has gone very wrong and we leave the
// FIFO alone rather than scrambling the order.
func (c *Conn) unappendPending(pc *pendingCmd) {
	c.pendingLock.Lock()
	defer c.pendingLock.Unlock()
	n := len(c.pending)
	if n > 0 && c.pending[n-1] == pc {
		c.pending = c.pending[:n-1]
	}
}

// popPending removes and returns the head of the pending FIFO, or nil
// if the FIFO is empty. Called only by the reader goroutine.
func (c *Conn) popPending() *pendingCmd {
	c.pendingLock.Lock()
	defer c.pendingLock.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	pc := c.pending[0]
	// Zero the head before shifting so pc isn't retained by the
	// underlying array.
	c.pending[0] = nil
	c.pending = c.pending[1:]
	return pc
}

// finishReader flips the Conn into a terminal error state and wakes
// any callers still waiting on pending commands. Safe to call from
// the reader goroutine; idempotent via closeOnce.
func (c *Conn) finishReader(err error) {
	c.closeOnce.Do(func() {
		// Set closeErr BEFORE the atomic flag so that any concurrent
		// reader of c.closed sees a valid closeErr.
		c.closeErr = err
		c.closed.Store(true)
		if c.cancel != nil {
			c.cancel() // release context resources; prevents leak when Close() is never called
		}
		_ = c.nc.Close() //nolint:errcheck // best-effort cleanup; underlying error already captured in c.closeErr

		c.pendingLock.Lock()
		orphans := c.pending
		c.pending = nil
		c.pendingLock.Unlock()

		for _, pc := range orphans {
			pc.result = cmdResult{err: err}
			close(pc.done)
		}
	})
}

// releaseSem returns one slot to the pipelining semaphore. Non-blocking;
// if the semaphore is already drained something has gone wrong but we
// don't want to deadlock the reader.
func (c *Conn) releaseSem() {
	select {
	case <-c.sem:
	default:
	}
}

// closeError returns the recorded reason the connection became unusable,
// or a generic ErrClosed if none was captured.
func (c *Conn) closeError() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	return ErrClosed
}

// classifyStatus maps an NNTP status code onto one of the sentinel
// errors so callers can branch without knowing the wire codes.
func classifyStatus(code int) error {
	switch code {
	case 430, 423:
		return ErrNoArticle
	case 480:
		return ErrAuthRequired
	case 481, 482:
		return ErrAuthRejected
	case 502, 503:
		return ErrServerUnavailable
	}
	if code >= 400 && code < 600 {
		return ErrTransient
	}
	return nil
}
