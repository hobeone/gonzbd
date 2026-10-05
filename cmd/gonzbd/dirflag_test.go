package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/config"
)

// runMainEnv makes the re-executed test binary call main() with the
// arguments it holds instead of running tests.
const runMainEnv = "GONZBD_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if args := os.Getenv(runMainEnv); args != "" {
		os.Args = append([]string{"gonzbd"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestDownloadDirFlagIsRejected pins that --download-dir is not a flag: an
// override held only in memory would be persisted by the next set_config save.
func TestDownloadDirFlagIsRejected(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), runMainEnv+"=--download-dir /elsewhere --version")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("--download-dir: err = %v, want exit status 2; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "flag provided but not defined: -download-dir") {
		t.Fatalf("output does not report an undefined flag:\n%s", out)
	}
}

func TestResolveDirs_UsesConfigDownloadDir(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.General.DownloadDir = "/data/dl"
	cfg.General.AdminDir = ""
	dl, admin, err := resolveDirs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dl != "/data/dl" || admin != "/data/dl/admin" {
		t.Fatalf("resolveDirs = %q, %q; want /data/dl, /data/dl/admin", dl, admin)
	}
	cfg.General.DownloadDir = ""
	if _, _, err := resolveDirs(cfg); err == nil {
		t.Fatal("empty download dir: want error")
	}
}
