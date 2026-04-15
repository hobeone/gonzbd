package downloader

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Resolve performs an asynchronous DNS lookup for host, returning all
// resolved IP addresses. The lookup runs in a goroutine so the caller
// can apply its own context deadline without blocking a thread for the
// duration of the OS resolver call.
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
	type result struct {
		addrs []netip.Addr
		err   error
	}

	ch := make(chan result, 1)
	go func() {
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		ch <- result{addrs: addrs, err: err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("downloader: resolve %s: %w", host, r.err)
		}
		if len(r.addrs) == 0 {
			return nil, fmt.Errorf("downloader: resolve %s: no addresses returned", host)
		}
		s.SetResolvedAddrs(r.addrs, time.Now())
		return r.addrs, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("downloader: resolve %s: %w", host, ctx.Err())
	}
}
