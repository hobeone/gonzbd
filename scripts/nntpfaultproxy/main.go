// Command nntpfaultproxy is a validation-only tool: a transparent NNTP
// relay that sits between gonzbd and a real Usenet provider and injects
// configurable faults (missing article, corrupted body, hung connection)
// into BODY/STAT responses for chosen message-IDs. It is never built into
// the production gonzbd binary — see scripts/nntpfaultproxy/README.md.
package main

import (
	"flag"
	"log/slog"
	"net"
	"os"
	"time"
)

func main() {
	configPath := flag.String("config", "", "path to fault proxy YAML config (required)")
	seed := flag.Int64("seed", 0, "RNG seed for rate-based fault rules (0 = derive from current time)")
	flag.Parse()

	if *configPath == "" {
		slog.Error("missing required -config flag")
		os.Exit(2)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		slog.Error("load config failed", "err", err)
		os.Exit(1)
	}

	s := *seed
	if s == 0 {
		s = time.Now().UnixNano()
	}

	log := slog.Default()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen failed", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}
	log.Info("nntpfaultproxy listening",
		"addr", cfg.Listen,
		"upstream_host", cfg.Upstream.Host,
		"upstream_port", cfg.Upstream.Port,
		"rules", len(cfg.Rules),
		"seed", s,
	)

	h := newConnHandler(cfg, log, uint64(s)) //nolint:gosec // validation tool, seed is not security-sensitive

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Error("accept failed", "err", err)
			return
		}
		go h.handle(conn)
	}
}
