// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() { //nocover: thin CLI entry point
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	cancel()
	os.Exit(code)
}

func printMsg(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...) //nolint:errcheck // CLI output writes to stderr/stdout
}

func printMsgln(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...) //nolint:errcheck // CLI output writes to stderr/stdout
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printMsgln(stderr, "usage: localnntp <subcommand> [flags]")
		printMsgln(stderr, "subcommands: gen, serve, verify, wait")
		return 1
	}

	switch args[0] {
	case "gen":
		return runGen(args[1:], stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "verify":
		return runVerify(ctx, args[1:], stdout, stderr)
	case "wait":
		return runWait(ctx, args[1:], stdout, stderr)
	default:
		printMsg(stderr, "unknown subcommand: %q\n", args[0])
		return 1
	}
}

func runGen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		dir            = fs.String("dir", "", "output directory for generated release files (required)")
		name           = fs.String("name", "release", "name of the release")
		sizeStr        = fs.String("size", "100M", "payload size (e.g. 100M, 1G)")
		volumes        = fs.Int("volumes", 10, "number of split RAR volumes")
		partSize       = fs.Int("part-size", 524288, "NNTP segment part size in bytes")
		par2Redundancy = fs.Int("par2-redundancy", 10, "PAR2 redundancy percentage")
		par2Volumes    = fs.Int("par2-volumes", 4, "number of PAR2 recovery volume files")
		seed           = fs.Uint64("seed", 1, "deterministic RNG seed")
		compress       = fs.Bool("compress", false, "compress payload with RAR")
		obfuscate      = fs.Bool("obfuscate", false, "obfuscate release filenames in NZB")
		sidecars       = fs.Bool("sidecars", true, "include NFO and sample sidecar files")
	)

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *dir == "" {
		printMsgln(stderr, "error: --dir is required")
		return 1
	}

	size, err := ParseSize(*sizeStr)
	if err != nil {
		printMsg(stderr, "error: parse size %q: %v\n", *sizeStr, err)
		return 1
	}

	cfg := GenConfig{
		OutDir:         *dir,
		Name:           *name,
		PayloadSize:    size,
		Volumes:        *volumes,
		PartSize:       *partSize,
		Par2Redundancy: *par2Redundancy,
		Par2Volumes:    *par2Volumes,
		Seed:           *seed,
		Compress:       *compress,
		Obfuscate:      *obfuscate,
		IncludeSidecar: *sidecars,
	}

	manifest, err := GenerateRelease(cfg)
	if err != nil {
		printMsg(stderr, "error generating release: %v\n", err)
		return 1
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		printMsg(stderr, "error writing manifest output: %v\n", err)
		return 1
	}

	return 0
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		listen   = fs.String("listen", "127.0.0.1:11190", "NNTP listen address")
		control  = fs.String("control", "127.0.0.1:11191", "HTTP control API listen address")
		rateMBps = fs.Float64("rate-mbps", 0.0, "bandwidth rate limit in MB/s (0 = unlimited)")
		seed     = fs.Uint64("seed", 1, "RNG seed for fault engine")
	)

	var (
		maxConns       = fs.Int("max-connections", 0, "max simultaneous client connections; extras get 502 (0 = unlimited)")
		refuse         = fs.Bool("refuse", false, "accept then immediately close without a greeting")
		rejectGreeting = fs.Bool("reject-greeting", false, "greet with 400 service temporarily unavailable and close")
		authFail       = fs.Bool("auth-fail", false, "answer AUTHINFO PASS with 481 and require auth for article commands")
	)

	var dirs []string
	fs.Func("dir", "release directory containing manifest.json (required, repeatable)", func(s string) error {
		dirs = append(dirs, s)
		return nil
	})

	var faultRules []FaultRule
	fs.Func("fault", "repeatable fault specification (e.g. drop:part01.rar:parts=1,2)", func(s string) error {
		rule, err := ParseFaultSpec(s)
		if err != nil {
			return err
		}
		faultRules = append(faultRules, rule)
		return nil
	})

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if len(dirs) == 0 {
		printMsgln(stderr, "error: --dir is required")
		return 1
	}

	sources := make([]ReleaseSource, 0, len(dirs))
	for _, d := range dirs {
		manifestPath := filepath.Join(d, "manifest.json")
		manifest, err := LoadManifest(manifestPath)
		if err != nil {
			printMsg(stderr, "error loading manifest from %s: %v\n", manifestPath, err)
			return 1
		}
		sources = append(sources, ReleaseSource{Dir: d, Manifest: manifest})
	}

	faults := NewFaultEngine(*seed, faultRules)
	limiter := NewRateLimiter(*rateMBps)

	srv, err := NewServer(ServerConfig{
		ReleaseDir:    sources[0].Dir,
		Manifest:      sources[0].Manifest,
		ExtraReleases: sources[1:],
		ListenAddr:    *listen,
		Faults:        faults,
		Limiter:       limiter,
		Conn: ConnSettings{
			MaxConnections: *maxConns,
			Refuse:         *refuse,
			RejectGreeting: *rejectGreeting,
			AuthFail:       *authFail,
		},
	})
	if err != nil {
		printMsg(stderr, "error creating server: %v\n", err)
		return 1
	}

	if err := srv.Start(); err != nil {
		printMsg(stderr, "error starting server: %v\n", err)
		return 1
	}
	defer func() { _ = srv.Close() }() //nolint:errcheck // best effort close on exit

	controlLn, err := net.Listen("tcp", *control)
	if err != nil {
		printMsg(stderr, "error starting control server: %v\n", err)
		return 1
	}

	controlHandler := NewControlHandler(srv)
	controlSrv := &http.Server{
		Handler:           controlHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
	}

	go func() {
		if err := controlSrv.Serve(controlLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			printMsg(stderr, "control server error: %v\n", err)
		}
	}()

	printMsg(stdout, "NNTP server listening on %s\nControl API listening on %s\n", srv.Addr(), controlLn.Addr())

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = controlSrv.Shutdown(shutdownCtx) //nolint:errcheck // best-effort shutdown on exit
	_ = srv.Close()                      //nolint:errcheck // best-effort close on exit

	return 0
}

func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		mode                 = fs.String("mode", "", "verification mode ('crash' or 'complete') (required)")
		manifestPath         = fs.String("manifest", "", "path to release manifest.json (required)")
		adminDir             = fs.String("admin-dir", "", "path to gonzbd admin directory (required)")
		downloadDir          = fs.String("download-dir", "", "path to gonzbd incomplete download directory (required)")
		completeDir          = fs.String("complete-dir", "", "path to gonzbd complete directory (required for complete mode)")
		jobID                = fs.String("job-id", "", "optional specific job ID to verify")
		serverURL            = fs.String("server", "", "optional HTTP control API URL (e.g. http://127.0.0.1:11191)")
		snapshotAs           = fs.String("snapshot-as", "", "optional snapshot name to record on crash")
		snapshot             = fs.String("snapshot", "", "optional snapshot name to compare against upon completion")
		expectArchiveCleanup = fs.Bool("expect-archive-cleanup", true, "fail if .rar or .par2 files remain in complete dir")
		expectSingleFetch    = fs.Bool("expect-single-fetch", false, "fail if any Message-ID was served clean more than once across --servers-for-fetch-check servers")
	)

	var fetchCheckURLs []string
	fs.Func("servers-for-fetch-check", "repeatable control URL whose /stats is summed for the cross-server fetch check", func(s string) error {
		fetchCheckURLs = append(fetchCheckURLs, s)
		return nil
	})

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *mode == "" || *manifestPath == "" || *adminDir == "" || *downloadDir == "" {
		printMsgln(stderr, "error: --mode, --manifest, --admin-dir, and --download-dir are required")
		return 1
	}

	cfg := VerifyConfig{
		Mode:               *mode,
		ManifestPath:       *manifestPath,
		AdminDir:           *adminDir,
		DownloadDir:        *downloadDir,
		CompleteDir:        *completeDir,
		JobID:              *jobID,
		ServerControlURL:   *serverURL,
		SnapshotAs:         *snapshotAs,
		CompareSnapshot:    *snapshot,
		SkipArchiveCleanup: !*expectArchiveCleanup,
		FetchCheckURLs:     fetchCheckURLs,
		ExpectSingleFetch:  *expectSingleFetch,
	}

	report, err := Verify(ctx, cfg)
	if err != nil {
		printMsg(stderr, "verification failed: %v\n", err)
		if report != nil {
			_ = json.NewEncoder(stdout).Encode(report) //nolint:errcheck // CLI output stream
		}
		return 1
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		printMsg(stderr, "error writing report: %v\n", err)
		return 1
	}

	return 0
}
