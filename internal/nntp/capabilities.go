package nntp

import (
	"bufio"
	"strings"
)

// Capabilities records which optional NNTP commands the server
// supports. The struct is intentionally tiny: we only care about
// BODY/STAT fallbacks for now, but other fields can be added as the
// downloader gains new selection strategies.
//
// A server that returns 5xx to CAPABILITIES produces a Capabilities
// with HasBody=true and HasStat=true (the conservative default);
// those commands are mandatory in RFC 3977 so assuming them present
// is safe.
type Capabilities struct {
	// HasBody reports whether BODY is in the capability list. Always
	// true for RFC-compliant servers; included so servers that lie
	// via "READER" + no BODY are visible to the caller.
	HasBody bool

	// HasStat reports whether STAT is advertised. STAT is cheaper
	// than BODY for existence probes and enables the duplicate-check
	// flow.
	HasStat bool

	// HasCompress reports whether XFEATURE COMPRESS GZIP is offered.
	// Not currently used; included because downloader.dispatch may
	// want it later for bandwidth savings on overview fetches.
	HasCompress bool

	// Raw is the verbatim list of capability lines, preserved so
	// debug logs can show exactly what the server advertised.
	Raw []string
}

// probeCapabilities issues CAPABILITIES and parses the dot-terminated
// response. Any protocol failure (including 5xx "unknown command")
// yields a conservative default: HasBody=true, HasStat=true. The
// function only reports an error by returning nil, which callers may
// treat as "probe failed, use defaults" — see Dial's handling.
//
// Writes go through bw (which the caller must flush-safely own) and
// responses come from br. Used synchronously during handshake, before
// the reader goroutine starts, so no pending-FIFO bookkeeping is
// required.
func probeCapabilities(bw *bufio.Writer, br *bufio.Reader) *Capabilities {
	if _, err := bw.WriteString("CAPABILITIES\r\n"); err != nil {
		return defaultCapabilities()
	}
	if err := bw.Flush(); err != nil {
		return defaultCapabilities()
	}
	line, err := readResponseLine(br)
	if err != nil {
		return defaultCapabilities()
	}
	code, _, err := parseStatus(line)
	if err != nil || code != 101 {
		// Either the server refused or sent garbage. Fall back to
		// the RFC-mandated defaults.
		return defaultCapabilities()
	}
	body, err := readDotStuffedBody(br)
	if err != nil {
		return defaultCapabilities()
	}
	return parseCapabilities(string(body))
}

// defaultCapabilities returns the conservative assumption used when
// probing fails: the connection supports the core RFC 3977 verbs.
func defaultCapabilities() *Capabilities {
	return &Capabilities{HasBody: true, HasStat: true}
}

// parseCapabilities interprets a CAPABILITIES body. Each line is a
// verb possibly followed by arguments; we care about the verb only.
// Unknown verbs are preserved in Raw for diagnostic logging.
func parseCapabilities(body string) *Capabilities {
	caps := &Capabilities{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		caps.Raw = append(caps.Raw, line)
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
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
		case "XFEATURE-COMPRESS":
			caps.HasCompress = true
		}
	}
	// If neither BODY nor STAT was enumerated but READER was not
	// present either, still fall back to the defaults. RFC 3977
	// guarantees these verbs when the server is in READER mode,
	// and SABnzbd has no use for a server that lacks them.
	if !caps.HasBody {
		caps.HasBody = true
	}
	if !caps.HasStat {
		caps.HasStat = true
	}
	return caps
}
