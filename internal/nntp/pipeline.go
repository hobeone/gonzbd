package nntp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
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

	if c.closed.Load() {
		c.releaseSem()
		return c.closeError()
	}

	c.pendingLock.Lock()
	c.pending = append(c.pending, pc)
	c.pendingLock.Unlock()

	if _, err := c.bw.Write(cmd); err != nil {
		c.unappendPendingLocked(pc)
		c.releaseSem()
		return fmt.Errorf("nntp: write: %w", err)
	}
	if err := c.bw.Flush(); err != nil {
		c.unappendPendingLocked(pc)
		c.releaseSem()
		return fmt.Errorf("nntp: flush: %w", err)
	}
	return nil
}

// unappendPendingLocked removes pc from the pending FIFO, used when a
// write fails after the append. pc is expected to be the most recent
// entry; if it isn't, something has gone very wrong and we leave the
// FIFO alone rather than scrambling the order.
func (c *Conn) unappendPendingLocked(pc *pendingCmd) {
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
		c.closed.Store(true)
		c.closeErr = err
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

// readResponseLine reads exactly one CRLF-terminated status line from
// br. Returns the line with CRLF (or bare LF) stripped. An unexpected
// EOF mid-line is returned as io.ErrUnexpectedEOF to make short-read
// diagnostics clearer.
func readResponseLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return "", io.EOF
		}
		if errors.Is(err, io.EOF) {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	line = trimCRLF(line)
	return line, nil
}

// parseStatus splits "NNN text..." into the numeric code and remaining
// text. The NNTP grammar mandates a single space between code and
// text, but some servers emit just the code with no trailing text on
// short responses (e.g. "200\r\n" — seen in the wild); handle that
// case too.
func parseStatus(line string) (code int, text string, err error) {
	if len(line) < 3 {
		return 0, "", fmt.Errorf("nntp: short response %q", line)
	}
	code, err = strconv.Atoi(line[:3])
	if err != nil {
		return 0, "", fmt.Errorf("nntp: non-numeric status %q", line)
	}
	if len(line) == 3 {
		return code, "", nil
	}
	if line[3] != ' ' {
		return 0, "", fmt.Errorf("nntp: malformed response %q", line)
	}
	return code, line[4:], nil
}

// readDotStuffedBody reads a multi-line response body from br per RFC
// 3977 §3.1.1. The body ends at a line containing only ".". Leading
// "." characters on other lines are dot-stuffed and must be removed
// (first byte dropped). CRLF on the wire is normalised to LF in the
// output.
//
// Maximum body size: 10 MB. Usenet articles are typically under 800 KB
// but this leaves generous headroom for oversized posts without risking
// a malicious server exhausting memory.
//
// Implementation: ReadSlice borrows the bufio reader's internal buffer
// rather than allocating a fresh slice per line, so the only heap
// traffic per call is the growing output buffer (~log2(body size)
// allocations from bytes.Buffer, not one per line).
func readDotStuffedBody(br *bufio.Reader) ([]byte, error) {
	const maxBody = 10 * 1024 * 1024
	var buf bytes.Buffer
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, fmt.Errorf("nntp: body line exceeds %d bytes", br.Size())
			}
			return nil, err
		}
		// Strip trailing LF (always present) and optional CR.
		end := len(line) - 1
		if end > 0 && line[end-1] == '\r' {
			end--
		}
		body := line[:end]

		// Terminator is a line whose content is exactly ".".
		if end == 1 && body[0] == '.' {
			return buf.Bytes(), nil
		}
		// Un-dotstuff: RFC 3977 §3.1.1 requires any line starting
		// with "." to be prefixed with an extra "." for transport;
		// strip that extra dot on receipt.
		if end > 0 && body[0] == '.' {
			body = body[1:]
		}
		if buf.Len()+len(body)+1 > maxBody {
			return nil, fmt.Errorf("nntp: body exceeds %d bytes", maxBody)
		}
		buf.Write(body)
		buf.WriteByte('\n')
	}
}

func trimCRLF(s string) string {
	n := len(s)
	if n >= 2 && s[n-2] == '\r' && s[n-1] == '\n' {
		return s[:n-2]
	}
	if n >= 1 && s[n-1] == '\n' {
		return s[:n-1]
	}
	return s
}
