package nntp

import (
	"errors"
	"fmt"
	"strings"
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
	_cmdDate         // reserved: protocol constant, not yet used
	_cmdQuit         // reserved: protocol constant, not yet used
	_cmdAuthInfoUser // reserved: protocol constant, not yet used
	_cmdAuthInfoPass // reserved: protocol constant, not yet used
)

// pendingCmd is a placeholder entry in the FIFO of outstanding
// commands. When the reader goroutine pops it off the FIFO, it fills
// result and closes done. Callers that cancel their context set
// orphaned=1; the reader still reads+discards the corresponding
// response so the connection stays in sync.
//
// msgID carries the Message-ID the command asked for, without angle
// brackets, for the command kinds whose success response echoes it
// back. It exists so the reader can establish that a response belongs
// to the request it was paired with, rather than trusting FIFO
// position alone — see runReader.
type pendingCmd struct {
	kind     cmdKind
	msgID    string        // requested Message-ID, no angle brackets; "" if the kind has none
	done     chan struct{} // closed by reader on completion
	result   cmdResult     // valid only after done is closed
	orphaned atomic.Bool
}

type cmdResult struct {
	code int
	line string // response line after the 3-digit code and space
	body []byte // populated for multi-line responses
	err  error  // non-nil on I/O or protocol errors

	// success reports whether code is the success code for the
	// command's kind. The reader computes it from successResponse and
	// publishes it here so callers do not restate the same code as a
	// literal: two expressions of "this response succeeded" can drift,
	// and the drift is silent in the direction that matters — a caller
	// accepting a body the reader never identity-checked.
	success bool
}

// successResponse reports the single status code that marks a
// successful response for a command kind, whether that response is
// followed by a dot-terminated multi-line body, and whether its status
// line echoes the requested Message-ID back (RFC 3977 §6.2: the
// article commands answer "NNN n message-id"). Any other code means a
// single-line response, usually an error.
//
// This is the sole owner of that table. The reader consults it for
// both decisions rather than keeping a second copy of the mapping,
// because a code listed in one place and omitted from the other would
// silently disable a check on exactly the responses it applies to.
func (k cmdKind) successResponse() (code int, hasBody, echoesMsgID bool) {
	switch k {
	case cmdBody:
		return 222, true, true
	case cmdArticle:
		return 220, true, true
	case cmdHead:
		return 221, true, true
	case cmdStat:
		return 223, false, true
	case cmdCapabilities:
		return 101, true, false
	}
	return 0, false, false
}

// messageIDFromStatusLine extracts the Message-ID from the text
// following the status code of an article response — canonically
// "n message-id" with optional trailing commentary. The angle-bracket
// wrapper is stripped so the result is directly comparable to the bare
// form pendingCmd.msgID holds.
//
// It finds the ID by its wrapper rather than by its position, and that
// is deliberate. RFC 3977 §6.2 brackets the Message-ID in every
// response that carries one, so the wrapper identifies it unambiguously
// wherever it sits; a positional rule has to assume the article-number
// field is present, and a server that omits it (non-conformant, but the
// shape real servers deviate into — see parseStatus, which already
// tolerates one such deviation) would yield a token that is not a
// Message-ID at all. That token then fails the comparison and kills the
// connection, so the positional reading fails in the worst direction:
// silently wrong, and fatal.
//
// A line carrying no bracketed token yields ok=false. That is not a
// mismatch but an unpairable response, and runReader distinguishes the
// two when reporting.
//
// Scanning is bounded by the response line, which readResponseLine caps
// at maxResponseLineLen, and allocates nothing: Cut and the Trim
// helpers all return substrings of their input.
func messageIDFromStatusLine(text string) (string, bool) {
	for rest := text; rest != ""; {
		var field string
		field, rest, _ = strings.Cut(rest, " ")
		if len(field) >= 2 && field[0] == '<' && field[len(field)-1] == '>' {
			if id := field[1 : len(field)-1]; id != "" {
				return id, true
			}
		}
	}
	return "", false
}

// newArticleCmd builds the FIFO entry and the wire bytes for a command
// naming a single article, from one Message-ID argument.
//
// It exists so those two never come from separate mentions of the same
// variable. A pendingCmd whose remembered identity disagrees with what
// its command actually asked for would make runReader's comparison
// assert the wrong thing, and the two call sites that build one had no
// structural reason to stay in step — the ID appeared twice, and the
// kinds successResponse marks as echoing already outnumber the kinds
// anything constructs. A third call site that omitted msgID would leave
// it empty and fail every response it ever received.
func newArticleCmd(kind cmdKind, verb, messageID string) (pc *pendingCmd, wire []byte) {
	return &pendingCmd{kind: kind, msgID: messageID, done: make(chan struct{})},
		fmt.Appendf(make([]byte, 0, 96), "%s <%s>\r\n", verb, messageID)
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

		expected, hasBody, echoesMsgID := pc.kind.successResponse()
		res := cmdResult{code: code, line: text, success: code == expected}

		// Establish that this response is an answer to the command it
		// was paired with, before reading anything further from the
		// wire. FIFO position alone does not establish that: a single
		// spurious response desyncs the queue, and every subsequent
		// article is then mis-attributed with bytes that are
		// internally consistent and pass every downstream check. The
		// comparison is octet-exact because RFC 3977 §3.6 makes
		// Message-IDs case-sensitive.
		//
		// Either failure is proof the queue can no longer be trusted,
		// which makes the NEXT response mis-paired too, so the
		// connection dies here rather than failing one article and
		// continuing. They are reported apart because they say
		// different things to whoever reads the log: one server
		// answered about the wrong article, the other sent a line this
		// command cannot be matched against at all.
		if res.success && echoesMsgID {
			switch got, ok := messageIDFromStatusLine(text); {
			case !ok:
				c.failPopped(pc, res, fmt.Errorf("%w: %d response to %q carries no Message-ID: %q",
					ErrDesynced, code, pc.msgID, text))
				return
			case got != pc.msgID:
				c.failPopped(pc, res, fmt.Errorf("%w: %d response to %q names %q instead",
					ErrDesynced, code, pc.msgID, got))
				return
			}
		}

		if res.success && hasBody {
			body, berr := readDotStuffedBody(c.br)
			if berr != nil {
				c.failPopped(pc, res, berr)
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

// failPopped ends the connection on err, having first delivered it to
// pc — the command the reader has already taken off the FIFO.
//
// That ordering is the whole reason this exists as a named step. The
// reader's usual teardown, wakeOrphans, drains the FIFO, and pc is no
// longer in it; nothing else will ever close its done channel, so
// finishReader alone leaves pc's caller blocked until its context
// expires. Every fatal path taken between popPending and the final
// close(pc.done) owes pc this, and the obligation is easy to miss
// because forgetting it produces a hang rather than an error.
//
// The caller must return immediately: the connection is dead and the
// reader loop must not go round again.
func (c *Conn) failPopped(pc *pendingCmd, res cmdResult, err error) {
	res.err = err
	pc.result = res
	close(pc.done)
	c.finishReader(err)
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

		c.wakeOrphans(err)
	})
}

// wakeOrphans drains the pending FIFO and signals all waiting callers
// with the given error. Called by both finishReader and Close inside
// closeOnce.Do, so it runs at most once per connection.
func (c *Conn) wakeOrphans(err error) {
	c.pendingLock.Lock()
	orphans := c.pending
	c.pending = nil
	c.pendingLock.Unlock()

	for _, pc := range orphans {
		pc.result = cmdResult{err: err}
		close(pc.done)
	}
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
