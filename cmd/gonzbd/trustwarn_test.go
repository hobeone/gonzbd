package main

import (
	"strings"
	"testing"
)

func TestNonLoopbackWithoutLocalRanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		host     string
		ranges   []string
		wantWarn bool
	}{
		{name: "loopback ipv4 no ranges", host: "127.0.0.1", wantWarn: false},
		{name: "loopback ipv6 no ranges", host: "::1", wantWarn: false},
		{name: "localhost name no ranges", host: "localhost", wantWarn: false},
		{name: "empty host no ranges", host: "", wantWarn: false},
		{name: "all-interfaces bind no ranges", host: "0.0.0.0", wantWarn: true},
		{name: "lan bind no ranges", host: "192.168.1.10", wantWarn: true},
		{name: "all-interfaces bind WITH ranges", host: "0.0.0.0", ranges: []string{"192.168.0.0/16"}, wantWarn: false},
		{name: "lan bind WITH ranges", host: "10.1.2.3", ranges: []string{"10.0.0.0/8"}, wantWarn: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			warn := nonLoopbackWithoutLocalRanges(tc.host, tc.ranges)
			if (warn != "") != tc.wantWarn {
				t.Errorf("nonLoopbackWithoutLocalRanges(host=%q, ranges=%v) warn=%q; wantWarn=%v", tc.host, tc.ranges, warn, tc.wantWarn)
			}
			if tc.wantWarn && !strings.Contains(warn, "local_ranges") {
				t.Errorf("warning should mention local_ranges: %q", warn)
			}
		})
	}
}

func TestListenerHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr string
		want string
	}{
		{addr: "127.0.0.1:4289", want: "127.0.0.1"},
		{addr: "0.0.0.0:8080", want: "0.0.0.0"},
		{addr: "[::1]:4289", want: "::1"},
		// A bare ":PORT" address (as produced by "--listen :4289") has no
		// host component, but net.Listen resolves it to the wildcard bind
		// (all interfaces) -- NOT loopback. Must normalize to a concrete
		// non-loopback marker, not pass the empty string through, or
		// nonLoopbackWithoutLocalRanges will misread "no host" as safe.
		{addr: ":4289", want: "0.0.0.0"},
		{addr: "no-port-here", want: "no-port-here"},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			if got := listenerHost(tc.addr); got != tc.want {
				t.Errorf("listenerHost(%q) = %q; want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestNonLoopbackWarnings exercises nonLoopbackWarnings directly -- this is
// the exact function serveMode calls with httpSrv.Addr/httpsSrv.Addr, so
// unlike testing listenerHost and nonLoopbackWithoutLocalRanges in
// isolation, this proves serveMode's actual effective-host selection,
// HTTPS-enabled gating, and same-host deduplication, not just that the two
// underlying helpers compose correctly in the abstract.
func TestNonLoopbackWarnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		httpAddr  string
		httpsAddr string
		httpsOn   bool
		ranges    []string
		wantWarns int
	}{
		{
			name:     "loopback http, https disabled: no warning",
			httpAddr: "127.0.0.1:4289", httpsAddr: "127.0.0.1:0", httpsOn: false,
			wantWarns: 0,
		},
		{
			// The bug this test guards: before nonLoopbackWarnings existed,
			// main.go checked cfg.General.Host directly, which --listen
			// never touches. A loopback config host with a --listen
			// override to a non-loopback address produced no warning even
			// though the daemon was actually listening non-loopback.
			name:     "config host loopback but --listen override is non-loopback",
			httpAddr: "0.0.0.0:4289", httpsAddr: "127.0.0.1:0", httpsOn: false,
			wantWarns: 1,
		},
		{
			// The bug the /96-adjacent listenerHost fix guards: "--listen
			// :4289" (bare port) has no host component but binds all
			// interfaces, not loopback.
			name:     "--listen bare port is a wildcard bind, not loopback",
			httpAddr: ":4289", httpsAddr: "127.0.0.1:0", httpsOn: false,
			wantWarns: 1,
		},
		{
			name:     "http loopback, https enabled and non-loopback",
			httpAddr: "127.0.0.1:4289", httpsAddr: "192.168.1.5:4443", httpsOn: true,
			wantWarns: 1,
		},
		{
			name:     "https non-loopback but disabled: not checked",
			httpAddr: "127.0.0.1:4289", httpsAddr: "192.168.1.5:4443", httpsOn: false,
			wantWarns: 0,
		},
		{
			name:     "both non-loopback, same host: deduplicated to one warning",
			httpAddr: "0.0.0.0:4289", httpsAddr: "0.0.0.0:4443", httpsOn: true,
			wantWarns: 1,
		},
		{
			name:     "both non-loopback, different hosts: two warnings",
			httpAddr: "0.0.0.0:4289", httpsAddr: "192.168.1.5:4443", httpsOn: true,
			wantWarns: 2,
		},
		{
			name:     "non-loopback but local_ranges configured: no warning",
			httpAddr: "0.0.0.0:4289", httpsAddr: "127.0.0.1:0", httpsOn: false,
			ranges: []string{"10.0.0.0/8"}, wantWarns: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nonLoopbackWarnings(tc.httpAddr, tc.httpsAddr, tc.httpsOn, tc.ranges)
			if len(got) != tc.wantWarns {
				t.Errorf("nonLoopbackWarnings(%q, %q, https=%v, %v) = %d warnings %v; want %d",
					tc.httpAddr, tc.httpsAddr, tc.httpsOn, tc.ranges, len(got), got, tc.wantWarns)
			}
		})
	}
}
