package config

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseLocalRanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      []string
		want    []string // canonical prefix strings
		wantErr bool
	}{
		{name: "empty", in: nil, want: nil},
		{name: "blank entries skipped", in: []string{"", "  "}, want: nil},
		{name: "cidr v4", in: []string{"10.0.0.0/8"}, want: []string{"10.0.0.0/8"}},
		{name: "cidr masked", in: []string{"192.168.1.5/24"}, want: []string{"192.168.1.0/24"}},
		{name: "bare v4 to host prefix", in: []string{"172.18.0.7"}, want: []string{"172.18.0.7/32"}},
		{name: "bare v6 to host prefix", in: []string{"fd00::1"}, want: []string{"fd00::1/128"}},
		{name: "cidr v6", in: []string{"fd00::/8"}, want: []string{"fd00::/8"}},
		{name: "mixed", in: []string{"10.0.0.0/8", "192.168.0.0/16"}, want: []string{"10.0.0.0/8", "192.168.0.0/16"}},
		{name: "invalid cidr", in: []string{"10.0.0.0/99"}, wantErr: true},
		{name: "invalid ip", in: []string{"not-an-ip"}, wantErr: true},
		{name: "garbage among valid", in: []string{"10.0.0.0/8", "bogus"}, wantErr: true},
		// IPv4-mapped CIDRs (::ffff:a.b.c.d/n) must normalize to plain IPv4
		// prefixes, or ipTrusted's family-mismatched Contains() check would
		// make the entry silently never match anything.
		{name: "ipv4-mapped cidr normalizes to plain ipv4", in: []string{"::ffff:192.168.1.0/120"}, want: []string{"192.168.1.0/24"}},
		{name: "ipv4-mapped host prefix normalizes", in: []string{"::ffff:10.1.2.3/128"}, want: []string{"10.1.2.3/32"}},
		{name: "ipv4-mapped prefix narrower than the /96 mapping boundary is rejected", in: []string{"::ffff:192.168.1.0/64"}, wantErr: true},
		{name: "ipv4-mapped prefix at exactly the /96 boundary is accepted", in: []string{"::ffff:0.0.0.0/96"}, want: []string{"0.0.0.0/0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLocalRanges(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLocalRanges(%v): want error, got nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLocalRanges(%v): unexpected error: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseLocalRanges(%v) = %v; want %v", tc.in, got, tc.want)
			}
			for i, p := range got {
				if p.String() != tc.want[i] {
					t.Errorf("prefix[%d] = %q; want %q", i, p.String(), tc.want[i])
				}
			}
		})
	}
}

func mustRanges(t *testing.T, entries ...string) []netip.Prefix {
	t.Helper()
	p, err := ParseLocalRanges(entries)
	if err != nil {
		t.Fatalf("ParseLocalRanges(%v): %v", entries, err)
	}
	return p
}

func TestIsTrustedRemote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		remoteAddr    string
		fh            ForwardedHeaders
		ranges        []string
		verifyXFF     bool
		forwardHeader TrustedForwardHeader
		want          bool
		// wantReasonContains, if set, asserts the rejection reason mentions
		// this substring — pins *why* a case is rejected, not just that it is.
		wantReasonContains string
	}{
		// Loopback is always trusted, regardless of ranges.
		{name: "loopback v4 default", remoteAddr: "127.0.0.1:5000", want: true},
		{name: "loopback v6 default", remoteAddr: "[::1]:5000", want: true},
		{name: "loopback 4-in-6", remoteAddr: "[::ffff:127.0.0.1]:5000", want: true},

		// Non-loopback is untrusted with the default (empty) ranges.
		{name: "private not trusted by default", remoteAddr: "192.168.1.10:5000", want: false, wantReasonContains: "not loopback"},
		{name: "docker bridge not trusted by default", remoteAddr: "172.18.0.5:5000", want: false},
		{name: "public not trusted by default", remoteAddr: "8.8.8.8:5000", want: false},

		// Explicit local_ranges allowlist the source.
		{name: "private trusted via range", remoteAddr: "192.168.1.10:5000", ranges: []string{"192.168.0.0/16"}, want: true},
		{name: "docker trusted via range", remoteAddr: "172.18.0.5:5000", ranges: []string{"172.18.0.0/16"}, want: true},
		{name: "bare host range match", remoteAddr: "172.18.0.5:5000", ranges: []string{"172.18.0.5"}, want: true},
		{name: "outside range untrusted", remoteAddr: "172.19.0.5:5000", ranges: []string{"172.18.0.0/16"}, want: false},
		{name: "private trusted via ipv4-mapped cidr range", remoteAddr: "192.168.1.10:5000", ranges: []string{"::ffff:192.168.0.0/112"}, want: true},

		// A forwarding header present but unverified fails closed, even from
		// an otherwise-trusted peer — this is the issue #94 fix: a reverse
		// proxy on the same host makes the peer look loopback/local, so its
		// mere presence with no verification must not be trusted.
		{name: "spoofed xff from untrusted peer still rejected (peer check first)", remoteAddr: "8.8.8.8:5000", fh: ForwardedHeaders{XForwardedFor: "127.0.0.1"}, want: false},
		{
			name: "trusted peer + xff present + verify off -> fails closed", remoteAddr: "192.168.1.10:5000",
			fh: ForwardedHeaders{XForwardedFor: "8.8.8.8"}, ranges: []string{"192.168.0.0/16"},
			want: false, wantReasonContains: "verify_xff_header is false",
		},
		{
			name: "trusted peer + forwarded present + verify off -> fails closed", remoteAddr: "192.168.1.10:5000",
			fh: ForwardedHeaders{Forwarded: "for=8.8.8.8"}, ranges: []string{"192.168.0.0/16"},
			want: false, wantReasonContains: "verify_xff_header is false",
		},
		{
			name: "trusted peer + x-real-ip present + verify off -> fails closed", remoteAddr: "192.168.1.10:5000",
			fh: ForwardedHeaders{XRealIP: "8.8.8.8"}, ranges: []string{"192.168.0.0/16"},
			want: false, wantReasonContains: "verify_xff_header is false",
		},
		{name: "trusted peer + no forwarding header + verify off -> trusted", remoteAddr: "192.168.1.10:5000", ranges: []string{"192.168.0.0/16"}, want: true},

		// verifyXFF: peer must be trusted AND every hop trusted.
		{name: "verify: untrusted peer rejected before xff", remoteAddr: "8.8.8.8:5000", fh: ForwardedHeaders{XForwardedFor: "127.0.0.1"}, verifyXFF: true, want: false},
		{name: "verify: trusted peer + trusted hop", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XForwardedFor: "192.168.1.50"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: true},
		{name: "verify: trusted peer + untrusted hop", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XForwardedFor: "8.8.8.8"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: false},
		{name: "verify: multi-hop all trusted", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XForwardedFor: "192.168.1.50, 192.168.1.51"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: true},
		{name: "verify: multi-hop one untrusted", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XForwardedFor: "192.168.1.50, 8.8.8.8"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: false},
		{name: "verify: no forwarding header at all ok", remoteAddr: "127.0.0.1:5000", verifyXFF: true, want: true},
		{name: "verify: unparseable hop rejected", remoteAddr: "127.0.0.1:5000", fh: ForwardedHeaders{XForwardedFor: "not-an-ip"}, verifyXFF: true, want: false},

		// Forwarded: (RFC 7239) — only the for= parameter is consulted.
		// All Forwarded: cases below explicitly select ForwardHeaderForwarded
		// — without it, the default XFF selector would find no XFF header
		// and fall back to peer-only trust, making these vacuously pass.
		{name: "verify: forwarded single hop trusted", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{Forwarded: "for=192.168.1.50;proto=https;by=192.168.1.1"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderForwarded, want: true},
		{name: "verify: forwarded single hop untrusted", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{Forwarded: "for=8.8.8.8"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderForwarded, want: false},
		{name: "verify: forwarded multi-element chain", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{Forwarded: "for=192.168.1.50, for=192.168.1.51"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderForwarded, want: true},
		{name: "verify: forwarded quoted ipv6 with port", remoteAddr: "[fd00::2]:5000", fh: ForwardedHeaders{Forwarded: `for="[fd00::1]:4711"`}, ranges: []string{"fd00::/8"}, verifyXFF: true, forwardHeader: ForwardHeaderForwarded, want: true},
		{name: "verify: forwarded with no for= param has no hops to check, falls back to peer trust", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{Forwarded: "by=192.168.1.1;proto=https"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderForwarded, want: true},

		// X-Real-IP — single address, no chain. Explicitly selects
		// ForwardHeaderXRealIP for the same reason as above.
		{name: "verify: x-real-ip trusted", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XRealIP: "192.168.1.50"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderXRealIP, want: true},
		{name: "verify: x-real-ip untrusted", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XRealIP: "8.8.8.8"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderXRealIP, want: false},

		// TrustedForwardHeader selects exactly one header to consult; the
		// other two are ignored for verification purposes even if present.
		// Empty forwardHeader defaults to X-Forwarded-For (back-compat).
		{name: "default selector consults xff, ignores forwarded/x-real-ip content", remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XForwardedFor: "192.168.1.50", Forwarded: "for=8.8.8.8", XRealIP: "8.8.8.8"}, ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: true},
		// Regression guard for the reviewed finding on issue #94: when the
		// operator has declared X-Real-IP as the proxy-managed header, an
		// attacker-injected X-Forwarded-For (even one that LOOKS like a
		// trusted chain) must NOT override the real signal. Before adding
		// TrustedForwardHeader, "presence order" picked XFF unconditionally,
		// so a forged XFF could win over a legitimate but untrusted
		// X-Real-IP and bypass verification entirely.
		{
			name:       "x-real-ip selector: forged trusted-looking xff is ignored, real x-real-ip is checked and rejected",
			remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XForwardedFor: "192.168.1.50", XRealIP: "8.8.8.8"},
			ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderXRealIP, want: false,
		},
		{
			name:       "x-real-ip selector: trusted x-real-ip accepted regardless of untrusted xff/forwarded present",
			remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XForwardedFor: "8.8.8.8", Forwarded: "for=8.8.8.8", XRealIP: "192.168.1.50"},
			ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderXRealIP, want: true,
		},
		{
			name:       "forwarded selector: untrusted forwarded rejected even with trusted-looking xff present",
			remoteAddr: "192.168.1.1:5000", fh: ForwardedHeaders{XForwardedFor: "192.168.1.50", Forwarded: "for=8.8.8.8"},
			ranges: []string{"192.168.0.0/16"}, verifyXFF: true, forwardHeader: ForwardHeaderForwarded, want: false,
		},

		// Malformed remote addresses.
		{name: "empty remote", remoteAddr: "", want: false},
		{name: "garbage remote", remoteAddr: "garbage", want: false},
		{name: "bare loopback no port", remoteAddr: "127.0.0.1", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ranges := mustRanges(t, tc.ranges...)
			got, reason := IsTrustedRemote(tc.remoteAddr, tc.fh, ranges, tc.verifyXFF, tc.forwardHeader)
			if got != tc.want {
				t.Errorf("IsTrustedRemote(%q, %+v, ranges=%v, verify=%v) = %v (reason %q); want %v",
					tc.remoteAddr, tc.fh, tc.ranges, tc.verifyXFF, got, reason, tc.want)
			}
			if tc.wantReasonContains != "" && !strings.Contains(reason, tc.wantReasonContains) {
				t.Errorf("reason = %q; want substring %q", reason, tc.wantReasonContains)
			}
			if tc.want && reason != "" {
				t.Errorf("trusted=true but reason = %q; want empty", reason)
			}
		})
	}
}

func TestTrustedForwardHeader_ValidAndNormalized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		h             TrustedForwardHeader
		wantValid     bool
		wantNormalize TrustedForwardHeader
	}{
		{name: "empty defaults to xff", h: "", wantValid: true, wantNormalize: ForwardHeaderXFF},
		{name: "lowercase xff", h: "x-forwarded-for", wantValid: true, wantNormalize: ForwardHeaderXFF},
		{name: "mixed case xff accepted (header names conventionally mixed-case)", h: "X-Forwarded-For", wantValid: true, wantNormalize: ForwardHeaderXFF},
		{name: "mixed case forwarded accepted", h: "Forwarded", wantValid: true, wantNormalize: ForwardHeaderForwarded},
		{name: "mixed case x-real-ip accepted", h: "X-Real-IP", wantValid: true, wantNormalize: ForwardHeaderXRealIP},
		{name: "unknown value rejected", h: "x-forwarded-host", wantValid: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.h.Valid(); got != tc.wantValid {
				t.Errorf("Valid() = %v; want %v", got, tc.wantValid)
			}
			if tc.wantValid {
				if got := tc.h.normalized(); got != tc.wantNormalize {
					t.Errorf("normalized() = %q; want %q", got, tc.wantNormalize)
				}
			}
		})
	}
}

func TestParseForwardedFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		in       string
		wantHops []string
		wantOK   bool
	}{
		{name: "single unquoted", in: "for=192.0.2.60", wantHops: []string{"192.0.2.60"}, wantOK: true},
		{name: "single with other params", in: "for=192.0.2.60;proto=http;by=203.0.113.43", wantHops: []string{"192.0.2.60"}, wantOK: true},
		{name: "multi element", in: "for=192.0.2.60, for=198.51.100.17", wantHops: []string{"192.0.2.60", "198.51.100.17"}, wantOK: true},
		{name: "quoted ipv6 with port", in: `for="[2001:db8:cafe::17]:4711"`, wantHops: []string{"2001:db8:cafe::17"}, wantOK: true},
		{name: "no for param", in: "by=203.0.113.43;proto=http", wantHops: nil, wantOK: false},
		{name: "empty", in: "", wantHops: nil, wantOK: false},
		// Unbracketed IPv4 with a port must have the port stripped (exactly
		// one ':' in the value).
		{name: "unbracketed ipv4 with port", in: "for=192.0.2.60:8080", wantHops: []string{"192.0.2.60"}, wantOK: true},
		// Unbracketed bare IPv6 (no port, no brackets) has 2+ colons, so it
		// must NOT be treated as "IP:port" — stripping would truncate a
		// valid address. This distinguishes the port-stripping branch's
		// exact "]" absent AND exactly-one-colon" condition from either half
		// being dropped.
		{name: "unbracketed bare ipv6 not mistaken for ip:port", in: "for=2001:db8::1", wantHops: []string{"2001:db8::1"}, wantOK: true},
		// Bracketed IPv6 with no port: "]" is present so the port-stripping
		// else-if must not even be considered (only the "]" branch fires).
		{name: "bracketed ipv6 no port", in: "for=[2001:db8::1]", wantHops: []string{"2001:db8::1"}, wantOK: true},
		// "]" as the very first character exercises the i>=0 boundary at
		// i==0 for the bracket-stripping branch.
		{name: "bracket at start of value", in: `for="]"`, wantHops: []string{""}, wantOK: true},
		// ":" as the very first character (no "]") exercises the i>=0
		// boundary at i==0 for the port-stripping branch.
		{name: "colon at start of value", in: "for=:8080", wantHops: []string{""}, wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hops, ok := parseForwardedFor(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("parseForwardedFor(%q) ok = %v; want %v", tc.in, ok, tc.wantOK)
			}
			if len(hops) != len(tc.wantHops) {
				t.Fatalf("parseForwardedFor(%q) hops = %v; want %v", tc.in, hops, tc.wantHops)
			}
			for i, h := range hops {
				if h != tc.wantHops[i] {
					t.Errorf("hop[%d] = %q; want %q", i, h, tc.wantHops[i])
				}
			}
		})
	}
}
