package main

import (
	"net"
	"strconv"
	"testing"
	"time"
)

func TestDialUpstream_Plaintext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			close(accepted)
			_ = c.Close()
		}
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	conn, err := dialUpstream(UpstreamConfig{Host: host, Port: port}, 2*time.Second)
	if err != nil {
		t.Fatalf("dialUpstream: %v", err)
	}
	defer conn.Close()

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("listener never accepted a connection")
	}
}

func TestDialUpstream_ConnectionRefused(t *testing.T) {
	// Port 1 is reserved (tcpmux) and nothing should be listening on
	// localhost there in any normal test environment.
	_, err := dialUpstream(UpstreamConfig{Host: "127.0.0.1", Port: 1}, 1*time.Second)
	if err == nil {
		t.Fatal("expected dial error for a port with nothing listening, got nil")
	}
}

func TestDialUpstream_TLSWithoutTLSServerFails(t *testing.T) {
	// Dialing a plaintext listener with SSL:true must fail the TLS
	// handshake rather than silently succeeding as a plaintext connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			buf := make([]byte, 256)
			_, _ = c.Read(buf) // drain the TLS ClientHello attempt; ignore errors
		}
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	_, err = dialUpstream(UpstreamConfig{Host: host, Port: port, SSL: true}, 2*time.Second)
	if err == nil {
		t.Fatal("expected TLS handshake error against a plaintext listener, got nil")
	}
}
