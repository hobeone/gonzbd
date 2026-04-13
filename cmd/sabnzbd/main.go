// Command sabnzbd is the entry point for the SABnzbd Go reimplementation.
//
// At this stage of development the binary only reports its build version and
// exits. Subsequent steps in the implementation plan introduce configuration
// loading, the HTTP API server, and the download pipeline.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

// Version is the build version of the sabnzbd binary. It is overridden at
// build time via -ldflags="-X main.Version=<value>".
var Version = "0.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *showVersion {
		fmt.Println(Version)
		return
	}

	logger.Info("sabnzbd starting", slog.String("version", Version))
	logger.Info("no functionality implemented yet; exiting")
}
