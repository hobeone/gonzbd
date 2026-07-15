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

// IsTrustedRemote reports whether a request originating from remoteAddr (the
// host:port form of http.Request.RemoteAddr) carrying the given
// X-Forwarded-For header value should be treated as a trusted source for
// issuing and accepting the web session cookie.
//
// A source is trusted only if its direct peer IP is loopback or falls within
// one of the configured local_ranges. When verifyXFF is true and the peer
// already qualifies, every hop in the X-Forwarded-For chain must ALSO be
// trusted; a single untrusted or unparseable hop makes the request untrusted.
//
// This mirrors SABnzbd's model: the X-Forwarded-For header is consulted only
// after the direct peer is already trusted, so a public client that appends a
// forged "X-Forwarded-For: 127.0.0.1" cannot elevate itself — its peer IP
// fails the check first. Loopback-only is the default (empty ranges): a
// non-loopback client receives no admin cookie unless its range is explicitly
// allowlisted.
func IsTrustedRemote(remoteAddr, xff string, ranges []netip.Prefix, verifyXFF bool) bool {
	peer, ok := parseHostIP(remoteAddr)
	if !ok {
		return false
	}
	if !ipTrusted(peer, ranges) {
		return false
	}
	if verifyXFF {
		for part := range strings.SplitSeq(xff, ",") {
			s := strings.TrimSpace(part)
			if s == "" {
				continue
			}
			ip, err := netip.ParseAddr(s)
			if err != nil {
				return false
			}
			if !ipTrusted(ip, ranges) {
				return false
			}
		}
	}
	return true
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

// parseHostIP extracts a netip.Addr from an http.Request.RemoteAddr value.
// It tolerates both "host:port" and bare-host forms. Returns ok=false when
// no valid IP can be parsed.
func parseHostIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}
