package config

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// ParseLocalRanges parses a list of local_ranges entries into netip prefixes.
// Each entry may be a CIDR ("10.0.0.0/8", "fd00::/8") or a bare IP
// ("192.168.1.5", "::1"), which is treated as a single-host prefix. Empty
// entries are skipped. Returns an error identifying the first entry that
// fails to parse so misconfiguration is surfaced at load time rather than
// silently disabling remote access.
func ParseLocalRanges(entries []string) ([]netip.Prefix, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return nil, fmt.Errorf("invalid local_range %q: %w", e, err)
			}
			p, err = normalizeMappedPrefix(p)
			if err != nil {
				return nil, fmt.Errorf("invalid local_range %q: %w", e, err)
			}
			out = append(out, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("invalid local_range %q: %w", e, err)
		}
		out = append(out, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
	}
	return out, nil
}

// normalizeMappedPrefix rewrites an IPv4-mapped IPv6 prefix (e.g.
// "::ffff:192.168.1.0/120") into its plain IPv4 form ("192.168.1.0/24").
// Without this, the prefix would never match anything: ipTrusted unmaps the
// peer address to plain IPv4 before checking containment, and
// netip.Prefix.Contains requires both addresses to share the same family —
// a 4-byte peer address can never be "contained" by a 16-byte IPv6 prefix,
// even one that is semantically an IPv4-mapped address. A CIDR whose prefix
// boundary falls inside the fixed ::ffff:0:0/96 mapping (bits < 96) can't be
// expressed as a plain IPv4 prefix at all, so that case is rejected rather
// than silently accepted as a dead entry. Prefixes that aren't IPv4-mapped
// pass through unchanged.
func normalizeMappedPrefix(p netip.Prefix) (netip.Prefix, error) {
	if !p.Addr().Is4In6() {
		return p, nil
	}
	if p.Bits() < 96 {
		return netip.Prefix{}, fmt.Errorf("IPv4-mapped prefix %s has fewer than 96 bits, cannot represent as IPv4", p)
	}
	return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96), nil
}

// ForwardedHeaders bundles the client-forwarding headers a reverse proxy may
// set on a request: X-Forwarded-For (a comma-separated hop chain), Forwarded
// (RFC 7239; only each element's for= parameter is consulted), and X-Real-IP
// (a single address, no chain). Any non-empty field is a signal that a proxy
// sits between the direct peer and the true client — see IsTrustedRemote.
type ForwardedHeaders struct {
	XForwardedFor string
	Forwarded     string
	XRealIP       string
}

// present reports whether any forwarding header was set on the request. This
// is deliberately "any of the three", independent of which one
// TrustedForwardHeader selects: even an unselected header's mere presence
// signals a proxy may be interposed, so IsTrustedRemote's fail-closed check
// (verifyXFF == false) fires regardless of selection.
func (f ForwardedHeaders) present() bool {
	return f.XForwardedFor != "" || f.Forwarded != "" || f.XRealIP != ""
}

// TrustedForwardHeader identifies which single client-forwarding header a
// deployment's reverse proxy is configured to set or overwrite reliably.
// Only this header is ever consulted for hop verification when verifyXFF is
// true — the other two are not considered, even if present, so a client that
// can inject an unmanaged header (e.g. a proxy that forwards X-Forwarded-For
// through untouched while only reliably overwriting X-Real-IP) cannot forge
// a competing, more-permissive chain in the header IsTrustedRemote actually
// checks. This mirrors nginx's ngx_http_realip_module real_ip_header
// directive, which requires the same explicit choice for the same reason.
type TrustedForwardHeader string

const (
	// ForwardHeaderXFF selects X-Forwarded-For. Also the effective default
	// when TrustedForwardHeader is empty, preserving this package's
	// pre-existing (XFF-only) behavior for configs written before this
	// field existed.
	ForwardHeaderXFF TrustedForwardHeader = "x-forwarded-for"
	// ForwardHeaderForwarded selects the RFC 7239 Forwarded header.
	ForwardHeaderForwarded TrustedForwardHeader = "forwarded"
	// ForwardHeaderXRealIP selects X-Real-IP.
	ForwardHeaderXRealIP TrustedForwardHeader = "x-real-ip"
)

// Valid reports whether h is the empty string (meaning "use the default,
// X-Forwarded-For") or one of the recognized selectors, compared
// case-insensitively (header names are conventionally written in mixed
// case, e.g. "X-Forwarded-For"). Used by config validation to reject typos
// at load time rather than silently falling back.
func (h TrustedForwardHeader) Valid() bool {
	switch TrustedForwardHeader(strings.ToLower(string(h))) {
	case "", ForwardHeaderXFF, ForwardHeaderForwarded, ForwardHeaderXRealIP:
		return true
	default:
		return false
	}
}

// normalized returns the effective selector, lowercased and defaulting empty
// to ForwardHeaderXFF.
func (h TrustedForwardHeader) normalized() TrustedForwardHeader {
	h = TrustedForwardHeader(strings.ToLower(string(h)))
	if h == "" {
		return ForwardHeaderXFF
	}
	return h
}

// hops returns the chain of forwarded client addresses to validate when
// verifyXFF is true, taken ONLY from the header selector identifies — the
// other two fields are ignored entirely, whether or not they're present; see
// TrustedForwardHeader. ok is false when the selected header is absent, or
// (for Forwarded specifically) present but with no for= parameter to
// extract — in both cases there is nothing to validate, so the caller falls
// back to trusting the already-confirmed peer alone. A selected header that
// DOES yield hop strings, but where a hop string itself fails to parse as an
// IP, is still caught by the caller's netip.ParseAddr check and fails closed.
func (f ForwardedHeaders) hops(selector TrustedForwardHeader) (hops []string, ok bool) {
	switch selector.normalized() {
	case ForwardHeaderForwarded:
		if f.Forwarded == "" {
			return nil, false
		}
		return parseForwardedFor(f.Forwarded)
	case ForwardHeaderXRealIP:
		if f.XRealIP == "" {
			return nil, false
		}
		return []string{strings.TrimSpace(f.XRealIP)}, true
	default: // ForwardHeaderXFF
		if f.XForwardedFor == "" {
			return nil, false
		}
		return splitHops(f.XForwardedFor), true
	}
}

// splitHops splits a comma-separated X-Forwarded-For value into trimmed,
// non-empty hop strings.
func splitHops(v string) []string {
	var hops []string
	for part := range strings.SplitSeq(v, ",") {
		if s := strings.TrimSpace(part); s != "" {
			hops = append(hops, s)
		}
	}
	return hops
}

// parseForwardedFor extracts the ordered list of for= node identifiers from
// an RFC 7239 Forwarded header value, one per comma-separated element
// (closest-first, mirroring X-Forwarded-For's convention). Handles quoted
// values and bracketed IPv6 literals with an optional trailing port (e.g.
// `for="[2001:db8::1]:4711"`). ok is false only when the header has no
// element with a for= parameter at all — a malformed for= value still
// produces a hop string that will fail netip.ParseAddr in the caller,
// keeping the fail-closed behavior for garbled headers.
func parseForwardedFor(v string) (hops []string, ok bool) {
	for elem := range strings.SplitSeq(v, ",") {
		for kv := range strings.SplitSeq(elem, ";") {
			k, val, found := strings.Cut(strings.TrimSpace(kv), "=")
			if !found || !strings.EqualFold(strings.TrimSpace(k), "for") {
				continue
			}
			val = strings.TrimSpace(val)
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
			}
			hops = append(hops, forwardedAddrOf(val))
			break
		}
	}
	return hops, len(hops) > 0
}

// forwardedAddrOf extracts the bare IP from a for= node identifier using
// strict net/netip parsing (not substring truncation), handling "ip",
// "ip:port", and bracketed "[ipv6]" / "[ipv6]:port" forms. A value that
// fails all three parses is returned UNCHANGED, so a malformed for=
// identifier (e.g. "127.0.0.1:garbage" or "127.0.0.1]junk") reaches the
// caller's netip.ParseAddr as-is and fails there, rather than being
// silently truncated into a valid-looking — and, for loopback, always
// trusted regardless of local_ranges — address.
func forwardedAddrOf(val string) string {
	if addr, err := netip.ParseAddr(val); err == nil {
		return addr.String()
	}
	if addrPort, err := netip.ParseAddrPort(val); err == nil {
		return addrPort.Addr().String()
	}
	if len(val) >= 2 && val[0] == '[' && val[len(val)-1] == ']' {
		if addr, err := netip.ParseAddr(val[1 : len(val)-1]); err == nil {
			return addr.String()
		}
	}
	return val
}

// IsTrustedRemote reports whether a request originating from remoteAddr (the
// host:port form of http.Request.RemoteAddr), carrying the given forwarding
// headers, should be treated as a trusted source for issuing and accepting
// the web session cookie. On rejection, reason explains why, suitable for an
// operator-facing log line; reason is empty when trusted is true.
//
// A source is trusted only if its direct peer IP is loopback or falls within
// one of the configured local_ranges. If any forwarding header is present but
// verifyXFF is false, the request fails closed: the header's mere presence
// means a proxy may be interposed, and an unverified peer address proves
// nothing about the real client. When verifyXFF is true and the peer already
// qualifies, every hop in the forwarding chain must ALSO be trusted; a single
// untrusted or unparseable hop makes the request untrusted.
//
// This mirrors SABnzbd's model: forwarding headers are consulted only after
// the direct peer is already trusted, so a public client that appends a
// forged "X-Forwarded-For: 127.0.0.1" cannot elevate itself — its peer IP
// fails the check first. Loopback-only is the default (empty ranges): a
// non-loopback client receives no admin cookie unless its range is explicitly
// allowlisted.
//
// forwardHeader selects which single header is consulted for hop
// verification (see TrustedForwardHeader); the other two are never consulted
// for verification purposes, though any of the three still counts toward
// the fh.present() fail-closed check above.
func IsTrustedRemote(remoteAddr string, fh ForwardedHeaders, ranges []netip.Prefix, verifyXFF bool, forwardHeader TrustedForwardHeader) (trusted bool, reason string) {
	peer, ok := parseHostIP(remoteAddr)
	if !ok {
		return false, "remote address could not be parsed"
	}
	if !ipTrusted(peer, ranges) {
		return false, "peer address is not loopback and not within a configured local_range"
	}
	if fh.present() && !verifyXFF {
		return false, "a forwarding header (X-Forwarded-For, Forwarded, or X-Real-IP) is present but " +
			"verify_xff_header is false: a reverse proxy may be interposed, so the peer address alone " +
			"cannot be trusted; set verify_xff_header: true and add the proxy's address to local_ranges " +
			"to trust it explicitly"
	}
	if verifyXFF {
		hops, present := fh.hops(forwardHeader)
		if !present || len(hops) == 0 {
			if fh.present() {
				// A different forwarding header is present even though the
				// selected one yielded nothing — that still signals a proxy
				// may be interposed (misconfigured trusted_forward_header,
				// or the selected header is malformed/blank), so this must
				// not silently degrade to peer-only trust.
				return false, fmt.Sprintf(
					"verify_xff_header is true but the selected header (%s) is absent, empty, or "+
						"contains no verifiable client address, even though a different forwarding "+
						"header is present; check trusted_forward_header matches what your proxy sets",
					forwardHeader.normalized())
			}
			return true, ""
		}
		for _, s := range hops {
			ip, err := netip.ParseAddr(strings.TrimSpace(s))
			if err != nil {
				return false, fmt.Sprintf("forwarding chain contains an unparseable hop %q", s)
			}
			if !ipTrusted(ip, ranges) {
				return false, fmt.Sprintf("forwarding chain hop %s is not within a trusted range", ip)
			}
		}
	}
	return true, ""
}

// ipTrusted reports whether ip is loopback or contained in one of ranges.
func ipTrusted(ip netip.Addr, ranges []netip.Prefix) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, p := range ranges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// SplitHostPortTolerant splits a "host:port" string into its host and port parts,
// stripping outer IPv6 brackets (e.g. "[::1]") if present on the no-port fallback path.
// Unlike net.SplitHostPort, it tolerates bare hostnames or bracketed IPv6 literals
// with no port by returning the host with an empty port rather than erroring.
func SplitHostPortTolerant(hostport string) (host, port string) {
	hostport = strings.TrimSpace(hostport)
	h, p, err := net.SplitHostPort(hostport)
	if err == nil {
		return strings.TrimSpace(h), strings.TrimSpace(p)
	}
	if len(hostport) >= 2 && hostport[0] == '[' && hostport[len(hostport)-1] == ']' {
		return strings.TrimSpace(hostport[1 : len(hostport)-1]), ""
	}
	return hostport, ""
}

// parseHostIP extracts a netip.Addr from an http.Request.RemoteAddr value.
// It tolerates both "host:port" and bare-host forms. Returns ok=false when
// no valid IP can be parsed.
func parseHostIP(remoteAddr string) (netip.Addr, bool) {
	host, _ := SplitHostPortTolerant(remoteAddr)
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}
