package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"time"
)

// dialUpstream opens a connection to the real NNTP server described by
// cfg. SSL connections use TLS 1.2+; SSLInsecure skips certificate
// verification, which is only ever appropriate for this local validation
// tool talking to a server you already trust by host/IP.
func dialUpstream(cfg UpstreamConfig, timeout time.Duration) (net.Conn, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: timeout}

	if !cfg.SSL {
		conn, err := dialer.Dial("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dial upstream %s: %w", addr, err)
		}
		return conn, nil
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.SSLInsecure, //nolint:gosec // explicit opt-in, validation-only tool
	}
	tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsConfig}
	conn, err := tlsDialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial upstream %s (tls): %w", addr, err)
	}
	return conn, nil
}
