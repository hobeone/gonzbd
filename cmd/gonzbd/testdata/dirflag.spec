pkg ./cmd/gonzbd/
run TestDownloadDirFlagIsRejected

[the flag is defined again]
file cmd/gonzbd/main.go
--- anchor
	verbose := flag.Bool("v", false, "verbose logging")
--- replace
	verbose := flag.Bool("v", false, "verbose logging")
	_ = flag.String("download-dir", "", "x")
--- end
