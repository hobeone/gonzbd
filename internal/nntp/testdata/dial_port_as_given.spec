pkg ./internal/nntp/
run ^TestNewDialOptions_PortAsGiven$

[the dial re-derives the port of an SSL server]
file internal/nntp/conn.go
--- anchor
		port:       cfg.Port,
--- replace
		port:       map[bool]int{true: 563, false: cfg.Port}[cfg.SSL],
--- end

[the dial re-derives a port of 0]
file internal/nntp/conn.go
--- anchor
		port:       cfg.Port,
--- replace
		port:       max(cfg.Port, 119),
--- end
