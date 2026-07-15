package main

import (
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
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
			g := config.GeneralConfig{Host: tc.host, LocalRanges: tc.ranges}
			warn := nonLoopbackWithoutLocalRanges(g)
			if (warn != "") != tc.wantWarn {
				t.Errorf("nonLoopbackWithoutLocalRanges(host=%q, ranges=%v) warn=%q; wantWarn=%v", tc.host, tc.ranges, warn, tc.wantWarn)
			}
			if tc.wantWarn && !strings.Contains(warn, "local_ranges") {
				t.Errorf("warning should mention local_ranges: %q", warn)
			}
		})
	}
}
