package unpack_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// captureCmd creates Options that capture the command line via OnCommand.
// Returns a pointer to the captured string.
func captureCmd(base unpack.Options) (unpack.Options, *string) {
	var captured string
	base.OnCommand = func(cmdLine string) {
		captured = cmdLine
	}
	base.UnrarCommand = "/nonexistent/binary"
	return base, &captured
}

// captureCmdSZ creates Options for 7z that capture the command line.
func captureCmdSZ(base unpack.Options) (unpack.Options, *string) {
	var captured string
	base.OnCommand = func(cmdLine string) {
		captured = cmdLine
	}
	base.SevenZipCommand = "/nonexistent/binary"
	return base, &captured
}

func dummyRarArchive() unpack.Archive {
	return unpack.Archive{
		Type:     unpack.RarArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.rar",
		Parts:    []string{"/tmp/does-not-exist.rar"},
	}
}

func dummySZArchive() unpack.Archive {
	return unpack.Archive{
		Type:     unpack.SevenZipArchive,
		Name:     "test",
		MainFile: "/tmp/does-not-exist.7z",
		Parts:    []string{"/tmp/does-not-exist.7z"},
	}
}

// TestUnRAR_OverwriteFiles verifies that OverwriteFiles=true adds -o+
// and that OverwriteFiles=false (default) adds -o- -or.
func TestUnRAR_OverwriteFiles(t *testing.T) {
	t.Parallel()

	t.Run("overwrite=true", func(t *testing.T) {
		t.Parallel()
		opts, captured := captureCmd(unpack.Options{OverwriteFiles: true})
		_, _ = unpack.UnRAR(t.Context(), slog.Default(), dummyRarArchive(), t.TempDir(), "", opts)
		if !strings.Contains(*captured, " -o+ ") && !strings.Contains(*captured, " -o+\n") {
			// Check if -o+ appears anywhere in the command
			if !strings.Contains(*captured, "-o+") {
				t.Errorf("OverwriteFiles=true: cmdline missing -o+: %q", *captured)
			}
		}
		if strings.Contains(*captured, "-o-") {
			t.Errorf("OverwriteFiles=true: cmdline should NOT contain -o-: %q", *captured)
		}
	})

	t.Run("overwrite=false", func(t *testing.T) {
		t.Parallel()
		opts, captured := captureCmd(unpack.Options{OverwriteFiles: false})
		_, _ = unpack.UnRAR(t.Context(), slog.Default(), dummyRarArchive(), t.TempDir(), "", opts)
		if !strings.Contains(*captured, "-o-") {
			t.Errorf("OverwriteFiles=false: cmdline missing -o-: %q", *captured)
		}
		if !strings.Contains(*captured, "-or") {
			t.Errorf("OverwriteFiles=false: cmdline missing -or: %q", *captured)
		}
	})
}

// TestUnRAR_IgnoreUnrarDatesPositive verifies that IgnoreUnrarDates=true
// with HasProblem=false adds -tsm- to the command line.
func TestUnRAR_IgnoreUnrarDatesPositive(t *testing.T) {
	t.Parallel()
	opts, captured := captureCmd(unpack.Options{
		IgnoreUnrarDates: true,
		HasProblem:       false,
	})
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), dummyRarArchive(), t.TempDir(), "", opts)
	if !strings.Contains(*captured, "-tsm-") {
		t.Errorf("IgnoreUnrarDates=true: cmdline missing -tsm-: %q", *captured)
	}
}

// TestUnRAR_FlatExtraction verifies that OneFolder=true uses 'e' command
// instead of 'x'.
func TestUnRAR_FlatExtraction(t *testing.T) {
	t.Parallel()

	t.Run("flat=true", func(t *testing.T) {
		t.Parallel()
		opts, captured := captureCmd(unpack.Options{OneFolder: true})
		_, _ = unpack.UnRAR(t.Context(), slog.Default(), dummyRarArchive(), t.TempDir(), "", opts)
		// The command should start with: /nonexistent/binary e -y ...
		parts := strings.Fields(*captured)
		if len(parts) < 2 {
			t.Fatalf("cmdline too short: %q", *captured)
		}
		if parts[1] != "e" {
			t.Errorf("OneFolder=true: mode = %q, want 'e': %q", parts[1], *captured)
		}
	})

	t.Run("flat=false_default", func(t *testing.T) {
		t.Parallel()
		opts, captured := captureCmd(unpack.Options{OneFolder: false})
		_, _ = unpack.UnRAR(t.Context(), slog.Default(), dummyRarArchive(), t.TempDir(), "", opts)
		parts := strings.Fields(*captured)
		if len(parts) < 2 {
			t.Fatalf("cmdline too short: %q", *captured)
		}
		if parts[1] != "x" {
			t.Errorf("OneFolder=false: mode = %q, want 'x': %q", parts[1], *captured)
		}
	})
}

// TestUnRAR_ExtraArgs verifies that ExtraArgs are appended to the command line.
func TestUnRAR_ExtraArgs(t *testing.T) {
	t.Parallel()
	opts, captured := captureCmd(unpack.Options{
		ExtraArgs: []string{"-mlp", "-ri10:5"},
	})
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), dummyRarArchive(), t.TempDir(), "", opts)
	if !strings.Contains(*captured, "-mlp") {
		t.Errorf("ExtraArgs: cmdline missing -mlp: %q", *captured)
	}
	if !strings.Contains(*captured, "-ri10:5") {
		t.Errorf("ExtraArgs: cmdline missing -ri10:5: %q", *captured)
	}
}

// TestUnRAR_NiceIoniceOptionsFlowThrough verifies that setting CmdCfg.Nice and
// CmdCfg.Ionice does not break command capture. It does NOT verify that nice/
// ionice wrapping is applied: formatCmdLine (cmdline.go) is only ever called
// with (bin, args, redact) — CmdCfg never reaches it — so nice/ionice prefixing
// happens purely inside exec.Cmd construction and is invisible to the captured
// string. There is no assertion here that could distinguish "nice/ionice
// applied" from "CmdCfg ignored"; this only guards against UnRAR erroring or
// skipping OnCommand when CmdCfg is set.
func TestUnRAR_NiceIoniceOptionsFlowThrough(t *testing.T) {
	t.Parallel()
	opts, captured := captureCmd(unpack.Options{
		CmdCfg: cmdutil.CmdConfig{
			Nice:   "-n 15",
			Ionice: "-c2 -n4",
		},
	})
	_, _ = unpack.UnRAR(t.Context(), slog.Default(), dummyRarArchive(), t.TempDir(), "", opts)
	if *captured == "" {
		t.Fatal("OnCommand was not called despite nice/ionice config")
	}
}

// TestSevenZip_OverwriteFiles verifies that OverwriteFiles=true adds -aoa
// and that the default adds -aou.
func TestSevenZip_OverwriteFiles(t *testing.T) {
	t.Parallel()

	t.Run("overwrite=true", func(t *testing.T) {
		t.Parallel()
		opts, captured := captureCmdSZ(unpack.Options{OverwriteFiles: true})
		_, _ = unpack.SevenZip(t.Context(), slog.Default(), dummySZArchive(), t.TempDir(), "", opts)
		if !strings.Contains(*captured, "-aoa") {
			t.Errorf("OverwriteFiles=true: cmdline missing -aoa: %q", *captured)
		}
	})

	t.Run("overwrite=false", func(t *testing.T) {
		t.Parallel()
		opts, captured := captureCmdSZ(unpack.Options{OverwriteFiles: false})
		_, _ = unpack.SevenZip(t.Context(), slog.Default(), dummySZArchive(), t.TempDir(), "", opts)
		if !strings.Contains(*captured, "-aou") {
			t.Errorf("OverwriteFiles=false: cmdline missing -aou: %q", *captured)
		}
	})
}

// TestSevenZip_FlatExtraction verifies 'e' vs 'x' mode for 7z.
func TestSevenZip_FlatExtraction(t *testing.T) {
	t.Parallel()

	t.Run("flat=true", func(t *testing.T) {
		t.Parallel()
		opts, captured := captureCmdSZ(unpack.Options{OneFolder: true})
		_, _ = unpack.SevenZip(t.Context(), slog.Default(), dummySZArchive(), t.TempDir(), "", opts)
		parts := strings.Fields(*captured)
		if len(parts) < 2 {
			t.Fatalf("cmdline too short: %q", *captured)
		}
		if parts[1] != "e" {
			t.Errorf("OneFolder=true: mode = %q, want 'e': %q", parts[1], *captured)
		}
	})

	t.Run("flat=false_default", func(t *testing.T) {
		t.Parallel()
		opts, captured := captureCmdSZ(unpack.Options{OneFolder: false})
		_, _ = unpack.SevenZip(t.Context(), slog.Default(), dummySZArchive(), t.TempDir(), "", opts)
		parts := strings.Fields(*captured)
		if len(parts) < 2 {
			t.Fatalf("cmdline too short: %q", *captured)
		}
		if parts[1] != "x" {
			t.Errorf("OneFolder=false: mode = %q, want 'x': %q", parts[1], *captured)
		}
	})
}
