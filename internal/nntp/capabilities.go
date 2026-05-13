package nntp

import (
	"bufio"
	"log/slog"
	"strconv"
	"strings"
)

// Capabilities records which optional NNTP commands the server
// supports. Populated by probeCapabilities from the server's
// CAPABILITIES response (RFC 3977 §5.2).
//
// A server that returns 5xx to CAPABILITIES produces a Capabilities
// with HasBody=true and HasStat=true (the conservative default);
// those commands are mandatory in RFC 3977 reader mode, so assuming
// them present is safe when the server refuses to answer.
type Capabilities struct {
	// HasBody reports whether the server supports the BODY command.
	// True when the CAPABILITIES response contains "READER" (which
	// implies BODY per RFC 3977 §5.3.2) or "BODY" explicitly.
	HasBody bool

	// HasStat reports whether the server supports the STAT command.
	// True when "READER" or "STAT" appears in CAPABILITIES.
	HasStat bool

	// HasOver reports whether the server supports OVER/XOVER.
	// Useful for future header-based filtering.
	HasOver bool

	// HasHDR reports whether the server supports HDR/XHDR.
	HasHDR bool

	// HasPost reports whether the server supports POST.
	HasPost bool

	// HasIHave reports whether the server supports IHAVE.
	HasIHave bool

	// HasCompress reports whether XFEATURE COMPRESS GZIP is offered.
	// Not currently used; included because downloader.dispatch may
	// want it later for bandwidth savings on overview fetches.
	HasCompress bool

	// Version is the capability version number from the VERSION line.
	// RFC 3977 mandates VERSION 2.
	Version int

	// Raw is the verbatim list of capability lines, preserved so
	// debug logs can show exactly what the server advertised.
	Raw []string
}

// probeCapabilities issues CAPABILITIES and parses the dot-terminated
// response. Any protocol failure (including 5xx "unknown command")
// yields a conservative default: HasBody=true, HasStat=true.
//
// Writes go through bw (which the caller must flush-safely own) and
// responses come from br. Used synchronously during handshake, before
// the reader goroutine starts, so no pending-FIFO bookkeeping is
// required.
func probeCapabilities(log *slog.Logger, bw *bufio.Writer, br *bufio.Reader) *Capabilities {
	if _, err := bw.WriteString("CAPABILITIES\r\n"); err != nil {
		log.Debug("probeCapabilities: write failed", "error", err)
		return defaultCapabilities()
	}
	if err := bw.Flush(); err != nil {
		log.Debug("probeCapabilities: flush failed", "error", err)
		return defaultCapabilities()
	}
	line, err := readResponseLine(br)
	if err != nil {
		log.Debug("probeCapabilities: read greeting failed", "error", err)
		return defaultCapabilities()
	}
	code, text, err := parseStatus(line)
	if err != nil || code != 101 {
		// Either the server refused or sent garbage. Fall back to
		// the RFC-mandated defaults.
		log.Debug("probeCapabilities: server rejected or malformed response", "code", code, "text", text, "error", err)
		return defaultCapabilities()
	}
	body, err := readDotStuffedBody(br)
	if err != nil {
		log.Debug("probeCapabilities: read body failed", "error", err)
		return defaultCapabilities()
	}
	return parseCapabilities(string(body))
}

// defaultCapabilities returns the conservative assumption used when
// probing fails: the connection supports the core RFC 3977 reader
// verbs (BODY, STAT). This is the safe default because BODY and STAT
// are mandatory when the server is in reader mode, and gonzbd only
// connects to reader-mode servers.
func defaultCapabilities() *Capabilities {
	return &Capabilities{HasBody: true, HasStat: true}
}

// parseCapabilities interprets a CAPABILITIES body. Each line is a
// verb possibly followed by arguments; we parse known verbs and
// preserve all lines in Raw for diagnostic logging.
//
// READER implies BODY + STAT + ARTICLE + HEAD per RFC 3977 §5.3.2.
// Individual verb lines (BODY, STAT, OVER, etc.) are also parsed so
// servers that enumerate commands explicitly are handled correctly.
//
// Unlike defaultCapabilities(), this function does NOT force-default
// any capabilities. If a server responds to CAPABILITIES but does not
// advertise BODY or STAT (and does not advertise READER), those
// fields remain false — the server genuinely doesn't support them.
func parseCapabilities(body string) *Capabilities {
	caps := &Capabilities{}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		caps.Raw = append(caps.Raw, line)
		parts := strings.SplitN(line, " ", 2)
		verb := strings.ToUpper(parts[0])
		switch verb {
		case "VERSION":
			if len(parts) > 1 {
				if v, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
					caps.Version = v
				}
			}
		case "READER":
			// READER implies BODY + STAT + ARTICLE + HEAD per RFC
			// 3977 §5.3.2. Many servers advertise this in lieu of
			// the individual verbs.
			caps.HasBody = true
			caps.HasStat = true
		case "BODY":
			caps.HasBody = true
		case "STAT":
			caps.HasStat = true
		case "OVER":
			caps.HasOver = true
		case "HDR":
			caps.HasHDR = true
		case "POST":
			caps.HasPost = true
		case "IHAVE":
			caps.HasIHave = true
		case "XFEATURE-COMPRESS":
			caps.HasCompress = true
		}
	}
	return caps
}
