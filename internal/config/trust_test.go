package config

import (
	"net/netip"
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
		name       string
		remoteAddr string
		xff        string
		ranges     []string
		verifyXFF  bool
		want       bool
	}{
		// Loopback is always trusted, regardless of ranges.
		{name: "loopback v4 default", remoteAddr: "127.0.0.1:5000", want: true},
		{name: "loopback v6 default", remoteAddr: "[::1]:5000", want: true},
		{name: "loopback 4-in-6", remoteAddr: "[::ffff:127.0.0.1]:5000", want: true},

		// Non-loopback is untrusted with the default (empty) ranges.
		{name: "private not trusted by default", remoteAddr: "192.168.1.10:5000", want: false},
		{name: "docker bridge not trusted by default", remoteAddr: "172.18.0.5:5000", want: false},
		{name: "public not trusted by default", remoteAddr: "8.8.8.8:5000", want: false},

		// Explicit local_ranges allowlist the source.
		{name: "private trusted via range", remoteAddr: "192.168.1.10:5000", ranges: []string{"192.168.0.0/16"}, want: true},
		{name: "docker trusted via range", remoteAddr: "172.18.0.5:5000", ranges: []string{"172.18.0.0/16"}, want: true},
		{name: "bare host range match", remoteAddr: "172.18.0.5:5000", ranges: []string{"172.18.0.5"}, want: true},
		{name: "outside range untrusted", remoteAddr: "172.19.0.5:5000", ranges: []string{"172.18.0.0/16"}, want: false},
		{name: "private trusted via ipv4-mapped cidr range", remoteAddr: "192.168.1.10:5000", ranges: []string{"::ffff:192.168.0.0/112"}, want: true},

		// X-Forwarded-For is ignored unless verifyXFF is on.
		{name: "spoofed xff ignored when verify off", remoteAddr: "8.8.8.8:5000", xff: "127.0.0.1", want: false},
		{name: "trusted peer, xff ignored when verify off", remoteAddr: "192.168.1.10:5000", xff: "8.8.8.8", ranges: []string{"192.168.0.0/16"}, want: true},

		// verifyXFF: peer must be trusted AND every hop trusted.
		{name: "verify: untrusted peer rejected before xff", remoteAddr: "8.8.8.8:5000", xff: "127.0.0.1", verifyXFF: true, want: false},
		{name: "verify: trusted peer + trusted hop", remoteAddr: "192.168.1.1:5000", xff: "192.168.1.50", ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: true},
		{name: "verify: trusted peer + untrusted hop", remoteAddr: "192.168.1.1:5000", xff: "8.8.8.8", ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: false},
		{name: "verify: multi-hop all trusted", remoteAddr: "192.168.1.1:5000", xff: "192.168.1.50, 192.168.1.51", ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: true},
		{name: "verify: multi-hop one untrusted", remoteAddr: "192.168.1.1:5000", xff: "192.168.1.50, 8.8.8.8", ranges: []string{"192.168.0.0/16"}, verifyXFF: true, want: false},
		{name: "verify: empty xff ok", remoteAddr: "127.0.0.1:5000", xff: "", verifyXFF: true, want: true},
		{name: "verify: unparseable hop rejected", remoteAddr: "127.0.0.1:5000", xff: "not-an-ip", verifyXFF: true, want: false},

		// Malformed remote addresses.
		{name: "empty remote", remoteAddr: "", want: false},
		{name: "garbage remote", remoteAddr: "garbage", want: false},
		{name: "bare loopback no port", remoteAddr: "127.0.0.1", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ranges := mustRanges(t, tc.ranges...)
			got := IsTrustedRemote(tc.remoteAddr, tc.xff, ranges, tc.verifyXFF)
			if got != tc.want {
				t.Errorf("IsTrustedRemote(%q, xff=%q, ranges=%v, verify=%v) = %v; want %v",
					tc.remoteAddr, tc.xff, tc.ranges, tc.verifyXFF, got, tc.want)
			}
		})
	}
}
