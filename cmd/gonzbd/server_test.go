package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/config"
)

func TestNewServersTimeouts(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("config.Default() failed: %v", err)
	}

	handler := http.NotFoundHandler()

	httpSrv, httpsSrv := newServers(cfg, handler)

	type serverTestCase struct {
		name string
		srv  *http.Server
	}

	testCases := []serverTestCase{
		{name: "HTTP Server", srv: httpSrv},
		{name: "HTTPS Server", srv: httpsSrv},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.srv == nil {
				t.Fatal("server is nil")
			}

			if tc.srv.ReadHeaderTimeout != 10*time.Second {
				t.Errorf("expected ReadHeaderTimeout = 10s, got %v", tc.srv.ReadHeaderTimeout)
			}
			if tc.srv.ReadTimeout != 30*time.Second {
				t.Errorf("expected ReadTimeout = 30s, got %v", tc.srv.ReadTimeout)
			}
			if tc.srv.WriteTimeout != 30*time.Second {
				t.Errorf("expected WriteTimeout = 30s, got %v", tc.srv.WriteTimeout)
			}
			if tc.srv.IdleTimeout != 120*time.Second {
				t.Errorf("expected IdleTimeout = 120s, got %v", tc.srv.IdleTimeout)
			}
		})
	}
}
