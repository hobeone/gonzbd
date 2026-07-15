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
		{addr: ":4289", want: ""},
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

// TestListenerHostReflectsOverride proves the warning must be evaluated
// against the *effective* listener address (httpSrv.Addr after --listen is
// applied), not the raw config-file general.host. Before this fix, main.go
// checked cfg.General.Host directly: a config with a loopback host but a
// --listen override binding non-loopback would silently produce no warning,
// even though the daemon is actually listening on the non-loopback address.
func TestListenerHostReflectsOverride(t *testing.T) {
	const configHost = "127.0.0.1"
	const overrideAddr = "0.0.0.0:4289" // as if --listen 0.0.0.0:4289 was passed

	if warn := nonLoopbackWithoutLocalRanges(configHost, nil); warn != "" {
		t.Fatalf("sanity check: the raw config host is loopback and should not warn on its own: %q", warn)
	}
	if warn := nonLoopbackWithoutLocalRanges(listenerHost(overrideAddr), nil); warn == "" {
		t.Error("the effective listener host (after a --listen override) is non-loopback; expected a warning, got none")
	}
}
