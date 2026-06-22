package main

import (
	"bufio"
	"log/slog"
	"math/rand/v2"
	"net"
	"time"
)

// connHandler relays one downstream (gonzbd) connection to a freshly
// dialed upstream connection, applying fault rules to BODY/STAT requests.
type connHandler struct {
	cfg *Config
	log *slog.Logger
	rng *rand.Rand
}

// newConnHandler builds a connHandler. seed drives rate-based fault
// matching; pass a fixed value for reproducible test/validation runs.
func newConnHandler(cfg *Config, log *slog.Logger, seed uint64) *connHandler {
	return &connHandler{
		cfg: cfg,
		log: log,
		rng: rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15)), //nolint:gosec // G404: deterministic seeded PRNG is intentional for reproducible fault injection
	}
}

// handle drives one downstream connection end to end: dials a fresh
// upstream connection, relays the greeting, then loops reading commands
// from downstream, applying any matching fault rule to BODY/STAT requests
// and passing everything else straight through. It owns closing both
// downstream and upstream.
func (h *connHandler) handle(downstream net.Conn) {
	defer downstream.Close() //nolint:errcheck // best-effort close

	upstream, err := dialUpstream(h.cfg.Upstream, 15*time.Second)
	if err != nil {
		h.log.Error("dial upstream failed", "err", err)
		return
	}
	defer upstream.Close() //nolint:errcheck // best-effort close

	dbr := bufio.NewReader(downstream)
	dbw := bufio.NewWriter(downstream)
	ubr := bufio.NewReader(upstream)
	ubw := bufio.NewWriter(upstream)

	if err := relayLine(ubr, dbw); err != nil {
		h.log.Debug("relay greeting failed", "err", err)
		return
	}

	for {
		line, err := dbr.ReadString('\n')
		if err != nil {
			return // downstream closed
		}

		kind, messageID := parseCommand(line)

		if kind == cmdBody || kind == cmdStat {
			if rule, matched := matchRule(h.cfg.Rules, messageID, h.rng); matched {
				if !h.applyFault(kind, messageID, rule, ubr, ubw, dbw) {
					return
				}
				continue
			}
		}

		if err := h.passthrough(line, kind, ubr, ubw, dbw); err != nil {
			h.log.Debug("passthrough failed", "err", err)
			return
		}
		if kind == cmdQuit {
			return
		}
	}
}

// passthrough forwards one client command to upstream verbatim and relays
// the response back, using isMultilineResponse to frame it correctly.
func (h *connHandler) passthrough(line string, kind commandKind, ubr *bufio.Reader, ubw, dbw *bufio.Writer) error {
	if _, err := ubw.WriteString(line); err != nil {
		return err
	}
	if err := ubw.Flush(); err != nil {
		return err
	}
	return relayResponse(ubr, dbw, kind)
}

// relayLine copies one CRLF-terminated line from src to dst and flushes.
// Used only for the initial greeting, which has no preceding command.
func relayLine(src *bufio.Reader, dst *bufio.Writer) error {
	line, err := src.ReadString('\n')
	if err != nil {
		return err
	}
	if _, err := dst.WriteString(line); err != nil {
		return err
	}
	return dst.Flush()
}

// relayResponse copies one full response — a single line, or a multiline
// block through its ".\r\n" terminator — from src to dst.
func relayResponse(src *bufio.Reader, dst *bufio.Writer, kind commandKind) error {
	line, err := src.ReadString('\n')
	if err != nil {
		return err
	}
	if _, err := dst.WriteString(line); err != nil {
		return err
	}
	if isMultilineResponse(kind, statusCode(line)) {
		for {
			l, err := src.ReadString('\n')
			if err != nil {
				return err
			}
			if _, err := dst.WriteString(l); err != nil {
				return err
			}
			if l == ".\r\n" {
				break
			}
		}
	}
	return dst.Flush()
}

// applyFault handles a BODY/STAT request matched by a fault rule. Returns
// true if the caller should continue reading the next command, false if
// the connection should be torn down.
func (h *connHandler) applyFault(kind commandKind, messageID string, rule Rule, ubr *bufio.Reader, ubw, dbw *bufio.Writer) bool {
	switch rule.Action {
	case "drop":
		return h.applyDrop(messageID, dbw)
	case "timeout":
		return h.applyTimeout(rule)
	case "corrupt":
		return h.applyCorrupt(kind, messageID, ubr, ubw, dbw, rule)
	default:
		return false
	}
}

// applyDrop responds with the same "430 no such article" a real server
// sends for a genuinely missing article, without contacting upstream at
// all — this drives gonzbd's real per-server retry/exhaustion logic
// exactly as a missing article would, for both BODY and STAT.
func (h *connHandler) applyDrop(messageID string, dbw *bufio.Writer) bool {
	h.log.Info("fault: drop", "message_id", messageID)
	if _, err := dbw.WriteString("430 no such article (fault injected)\r\n"); err != nil {
		return false
	}
	return dbw.Flush() == nil
}

// applyTimeout never responds at all, holding the connection open for
// rule.TimeoutAfter (default 60s) and then closing it without writing
// anything — simulating a hung/dead connection so gonzbd's own idle
// read-deadline and bad-connection handling get exercised.
func (h *connHandler) applyTimeout(rule Rule) bool {
	d := rule.TimeoutAfter
	if d <= 0 {
		d = 60 * time.Second
	}
	h.log.Info("fault: timeout", "duration", d)
	time.Sleep(d)
	return false
}

// applyCorrupt fetches the real response from upstream, then — for a
// successful BODY only — flips rule.CorruptBytes random bytes in the raw,
// still dot-stuffed body before relaying it downstream. A corrupt rule
// matching a STAT (which carries no body) passes the response through
// unchanged.
func (h *connHandler) applyCorrupt(kind commandKind, messageID string, ubr *bufio.Reader, ubw, dbw *bufio.Writer, rule Rule) bool {
	h.log.Info("fault: corrupt", "message_id", messageID)

	cmd := "BODY <" + messageID + ">\r\n"
	if kind == cmdStat {
		cmd = "STAT <" + messageID + ">\r\n"
	}
	if _, err := ubw.WriteString(cmd); err != nil {
		return false
	}
	if err := ubw.Flush(); err != nil {
		return false
	}

	statusLine, err := ubr.ReadString('\n')
	if err != nil {
		return false
	}
	if _, err := dbw.WriteString(statusLine); err != nil {
		return false
	}
	if !isMultilineResponse(kind, statusCode(statusLine)) {
		return dbw.Flush() == nil
	}

	var bodyLines [][]byte
	for {
		l, err := ubr.ReadString('\n')
		if err != nil {
			return false
		}
		if l == ".\r\n" {
			break
		}
		bodyLines = append(bodyLines, []byte(l))
	}

	n := rule.CorruptBytes
	if n <= 0 {
		n = 1
	}
	corruptLines(h.rng, bodyLines, n)

	for _, l := range bodyLines {
		if _, err := dbw.Write(l); err != nil {
			return false
		}
	}
	if _, err := dbw.WriteString(".\r\n"); err != nil {
		return false
	}
	return dbw.Flush() == nil
}

// corruptLines flips n random bytes across the data lines of a
// dot-stuffed NNTP body, in place. Each flip picks a random line and a
// random byte within it, so corruption can land anywhere in the article —
// including, occasionally, the yEnc header/trailer lines, which is itself
// a realistic (if different) fault: a total decode failure for that
// article rather than a content-level CRC mismatch.
func corruptLines(rng *rand.Rand, lines [][]byte, n int) {
	if len(lines) == 0 {
		return
	}
	for range n {
		line := lines[rng.IntN(len(lines))]
		if len(line) == 0 {
			continue
		}
		bi := rng.IntN(len(line))
		line[bi] ^= 0xFF
	}
}
