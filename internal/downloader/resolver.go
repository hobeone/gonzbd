package downloader

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Resolve performs a DNS lookup for host, returning all resolved IP addresses.
//
// Spec §3.7: "Fastest address selected (first successful probe), cached
// per-server for the session, re-resolved on reconnect after timeout."
// This function implements the lookup half; callers are responsible for
// caching via Server.SetResolvedAddrs and for selecting addrs[0] when
// opening a connection.
//
// Both IPv4 and IPv6 addresses are returned (LookupNetIP "ip" network).
// The order matches the OS resolver's preference. Callers that want a
// specific family should filter the result slice themselves.
//
// On success the result is cached on s and the addresses are returned.
// On error s's cache is not updated and the error is returned.
func Resolve(ctx context.Context, s *Server, host string) ([]netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("downloader: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("downloader: resolve %s: no addresses returned", host)
	}
	s.SetResolvedAddrs(addrs, time.Now())
	return addrs, nil
}
