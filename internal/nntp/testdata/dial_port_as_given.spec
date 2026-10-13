pkg ./internal/nntp/
run ^(TestNewDialOptions_PortAsGiven|TestNewDialOptions_TakesCheckedValues)$

[the dial re-derives the port of an SSL server]
file internal/nntp/conn.go
--- anchor
		port:       cfg.Port,
--- replace
		port:       map[bool]int{true: 563, false: cfg.Port}[cfg.SSL],
--- end

[the dial replaces the checked timeout with a fixed one]
file internal/nntp/conn.go
--- anchor
		dialer:     &net.Dialer{Timeout: time.Duration(cfg.Timeout) * time.Second},
--- replace
		dialer:     &net.Dialer{Timeout: 60 * time.Second},
--- end

[the dial replaces the checked pipelining depth with a fixed one]
file internal/nntp/conn.go
--- anchor
		pipelining: cfg.PipeliningRequests,
--- replace
		pipelining: 1,
--- end
