package main

import "strings"

// commandKind identifies which NNTP command a line represents, for the
// fixed, small set internal/nntp's client actually issues (confirmed via
// internal/nntp/conn.go and internal/nntp/capabilities.go: CAPABILITIES,
// AUTHINFO USER/PASS, BODY, STAT, QUIT — never GROUP or ARTICLE).
type commandKind int

const (
	cmdOther commandKind = iota
	cmdCapabilities
	cmdAuthInfo
	cmdBody
	cmdStat
	cmdQuit
)

// parseCommand classifies a command line and extracts its message-ID
// argument for BODY/STAT (without angle brackets). Unrecognised commands
// classify as cmdOther with an empty messageID — callers still relay them
// verbatim, they're just never subject to fault rules.
func parseCommand(line string) (kind commandKind, messageID string) {
	trimmed := strings.TrimRight(line, "\r\n")
	upper := strings.ToUpper(trimmed)
	switch {
	case upper == "CAPABILITIES":
		return cmdCapabilities, ""
	case strings.HasPrefix(upper, "AUTHINFO "):
		return cmdAuthInfo, ""
	case strings.HasPrefix(upper, "BODY "):
		return cmdBody, extractMessageID(trimmed[len("BODY "):])
	case strings.HasPrefix(upper, "STAT "):
		return cmdStat, extractMessageID(trimmed[len("STAT "):])
	case upper == "QUIT":
		return cmdQuit, ""
	default:
		return cmdOther, ""
	}
}

// extractMessageID strips optional angle brackets, mirroring the wire
// format gonzbd's client sends ("<id>").
func extractMessageID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>' {
		return s[1 : len(s)-1]
	}
	return s
}

// isMultilineResponse reports whether a response to the given command kind
// with the given status code is a dot-terminated multi-line block (true)
// or a single status line (false). CAPABILITIES is always multiline; BODY
// is multiline only on success (222); everything else this proxy handles
// (STAT, AUTHINFO, QUIT) is always single-line per RFC 3977.
func isMultilineResponse(kind commandKind, code int) bool {
	switch kind {
	case cmdCapabilities:
		return true
	case cmdBody:
		return code == 222
	default:
		return false
	}
}

// statusCode extracts the 3-digit leading status code from a response
// line, e.g. "222 0 <id> body follows" -> 222. Returns 0 if the line
// doesn't start with exactly 3 ASCII digits.
func statusCode(line string) int {
	if len(line) < 3 {
		return 0
	}
	code := 0
	for i := range 3 {
		c := line[i]
		if c < '0' || c > '9' {
			return 0
		}
		code = code*10 + int(c-'0')
	}
	return code
}
