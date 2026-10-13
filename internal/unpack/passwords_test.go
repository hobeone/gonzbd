package unpack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/testutil"
)

func TestAllPasswords_DeduplicatesAndOrders(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want []string
	}{
		{
			name: "empty",
			opts: Options{},
			want: nil,
		},
		{
			name: "single_password",
			opts: Options{Passwords: []string{"secret"}},
			want: []string{"secret"},
		},
		{
			name: "passwords_list",
			opts: Options{Passwords: []string{"a", "b", "c"}},
			want: []string{"a", "b", "c"},
		},
		{
			name: "empty_strings_filtered",
			opts: Options{Passwords: []string{"", "real", ""}},
			want: []string{"real"},
		},
		{
			name: "duplicates_in_list",
			opts: Options{Passwords: []string{"a", "b", "a", "c", "b"}},
			want: []string{"a", "b", "c"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := allPasswords(tc.opts)
			if len(got) != len(tc.want) {
				t.Fatalf("allPasswords() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("allPasswords()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestIsUnrarWrongPassword(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		output   string
		want     bool
	}{
		{"incorrect_password", 2, "Incorrect password for file.rar", true},
		{"specified_password", 2, "The specified password is incorrect.", true},
		{"checksum_encrypted", 2, "Checksum error in the encrypted file foo.dat", true},
		{"crc_failed_encrypted", 2, "Encrypted file: CRC failed in data.bin", true},
		{"normal_crc_error", 3, "CRC failed in data.bin", false},
		{"success", 0, "All OK", false},
		{"other_error", 7, "Cannot create output/path", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isUnrarWrongPassword(tc.exitCode, tc.output)
			if got != tc.want {
				t.Errorf("isUnrarWrongPassword(%d, %q) = %v, want %v",
					tc.exitCode, tc.output, got, tc.want)
			}
		})
	}
}

func TestIs7zWrongPassword(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		output   string
		want     bool
	}{
		{"wrong_password_question", 2, "Data Error in encrypted file. Wrong password?", true},
		{"wrong_password_short", 2, "Wrong password?", true},
		{"data_error_encrypted", 2, "Data Error in encrypted file foo.7z", true},
		{"normal_data_error", 2, "Data error in file.7z", false},
		{"success", 0, "Everything is Ok", false},
		{"other_error", 7, "Cannot open archive", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := is7zWrongPassword(tc.exitCode, tc.output)
			if got != tc.want {
				t.Errorf("is7zWrongPassword(%d, %q) = %v, want %v",
					tc.exitCode, tc.output, got, tc.want)
			}
		})
	}
}

func TestWithPasswords_Mocked(t *testing.T) {
	ctx := context.Background()
	log := slog.Default()
	archive := Archive{MainFile: "test.rar"}
	outDir := t.TempDir()

	t.Run("no passwords - calls extract once", func(t *testing.T) {
		calls := 0
		extract := func(ctx context.Context, log *slog.Logger, archive Archive, outDir string, password string, opts Options) (Result, error) {
			calls++
			if password != "" {
				t.Errorf("expected no password, got %q", password)
			}
			return Result{ExitCode: 0}, nil
		}

		opts := Options{}
		res, err := withPasswords(ctx, log, archive, outDir, opts, extract, nil, "mock")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if calls != 1 {
			t.Errorf("expected 1 call, got %d", calls)
		}
		if res.ExitCode != 0 {
			t.Errorf("expected ExitCode 0, got %d", res.ExitCode)
		}
	})

	t.Run("success on first password", func(t *testing.T) {
		calls := 0
		extract := func(ctx context.Context, log *slog.Logger, archive Archive, outDir string, password string, opts Options) (Result, error) {
			calls++
			if password != "pass1" {
				t.Errorf("expected pass1, got %q", password)
			}
			return Result{ExitCode: 0}, nil
		}

		opts := Options{Passwords: []string{"pass1", "pass2"}}
		_, err := withPasswords(ctx, log, archive, outDir, opts, extract, nil, "mock")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if calls != 1 {
			t.Errorf("expected 1 call, got %d", calls)
		}
	})

	t.Run("retry on wrong password then success", func(t *testing.T) {
		calls := 0
		extract := func(ctx context.Context, log *slog.Logger, archive Archive, outDir string, password string, opts Options) (Result, error) {
			calls++
			if calls == 1 {
				if password != "wrong" {
					t.Errorf("first attempt expected 'wrong', got %q", password)
				}
				return Result{ExitCode: 2, Output: "wrong password!"}, errors.New("extract failed")
			}
			if calls == 2 {
				if password != "right" {
					t.Errorf("second attempt expected 'right', got %q", password)
				}
				return Result{ExitCode: 0}, nil
			}
			return Result{}, fmt.Errorf("unexpected call %d", calls)
		}

		isWrongPW := func(exitCode int, output string) bool {
			return exitCode == 2 && strings.Contains(output, "wrong password")
		}

		opts := Options{Passwords: []string{"wrong", "right"}}
		res, err := withPasswords(ctx, log, archive, outDir, opts, extract, isWrongPW, "mock")
		if err != nil {
			t.Fatalf("expected success, got error %v", err)
		}
		if calls != 2 {
			t.Errorf("expected 2 calls, got %d", calls)
		}
		if res.ExitCode != 0 {
			t.Errorf("expected ExitCode 0, got %d", res.ExitCode)
		}
	})

	t.Run("exhaust all passwords", func(t *testing.T) {
		calls := 0
		extract := func(ctx context.Context, log *slog.Logger, archive Archive, outDir string, password string, opts Options) (Result, error) {
			calls++
			return Result{ExitCode: 2, Output: "wrong!"}, errors.New("extract failed")
		}

		isWrongPW := func(exitCode int, output string) bool {
			return true
		}

		opts := Options{Passwords: []string{"wrong1", "wrong2"}}
		_, err := withPasswords(ctx, log, archive, outDir, opts, extract, isWrongPW, "mock")
		if !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("expected ErrWrongPassword, got %v", err)
		}
		if calls != 2 {
			t.Errorf("expected 2 calls, got %d", calls)
		}
	})

	t.Run("system error aborts retry loop", func(t *testing.T) {
		calls := 0
		sysErr := errors.New("system error")
		extract := func(ctx context.Context, log *slog.Logger, archive Archive, outDir string, password string, opts Options) (Result, error) {
			calls++
			// ExitCode = 0, Reason = FailUnknown means system error
			return Result{ExitCode: 0, Reason: FailUnknown}, sysErr
		}

		opts := Options{Passwords: []string{"wrong1", "wrong2"}}
		_, err := withPasswords(ctx, log, archive, outDir, opts, extract, nil, "mock")
		if !errors.Is(err, sysErr) {
			t.Fatalf("expected system error, got %v", err)
		}
		if calls != 1 {
			t.Errorf("expected 1 call (no retry on system error), got %d", calls)
		}
	})
}

// TestWithPasswords_GoNativeWrongPasswordRetries is the red-green guard for the
// C3 fix (b84d74a). A Go-native extractor signals a wrong password with
// ExitCode==0, Reason==FailWrongPassword, AND a non-nil error. The system-error
// early-return must NOT fire for that shape (it is an extraction failure, not a
// failure to start the process), so withPasswords must retry the next password.
// Before the fix (the guard lacked `&& res.Reason == FailUnknown`), the first
// wrong password was returned as a system error and the correct one was never
// tried — this test fails on that reverted code.
func TestWithPasswords_GoNativeWrongPasswordRetries(t *testing.T) {
	ctx := context.Background()
	log := slog.Default()
	archive := Archive{MainFile: "test.7z"}
	outDir := t.TempDir()

	calls := 0
	extract := func(_ context.Context, _ *slog.Logger, _ Archive, _ string, password string, _ Options) (Result, error) {
		calls++
		if password == "correct" {
			return Result{ExitCode: 0, ExtractedFiles: []string{"file.txt"}}, nil
		}
		// Go-native extractor shape: ExitCode 0 + classified Reason + non-nil err.
		return Result{ExitCode: 0, Reason: FailWrongPassword}, errors.New("wrong password")
	}

	opts := Options{Passwords: []string{"wrong", "correct"}}
	res, err := withPasswords(ctx, log, archive, outDir, opts, extract, nil, "mock")
	if err != nil {
		t.Fatalf("expected retry to succeed with the correct password, got error: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 attempts (retry past wrong password), got %d", calls)
	}
	if len(res.ExtractedFiles) != 1 {
		t.Errorf("expected extracted files from the successful attempt, got %v", res.ExtractedFiles)
	}
}

// recordingUnrar writes a fake unrar that appends its -p argument to a log
// and extracts one file, and returns the script's path and the log's.
func recordingUnrar(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "passwords.log")
	bin = filepath.Join(dir, "unrar")
	testutil.WriteExecutable(t, bin, fmt.Sprintf(`#!/bin/sh
for a in "$@"; do case "$a" in -p*) echo "$a" >> %q;; esac; done
eval "OUTDIR=\${$#}"
echo x > "${OUTDIR}out.txt"
exit 0
`, logPath))
	return bin, logPath
}

// A password the archive's own check value proves wrong never reaches
// unrar: UnRAR refuses it as a wrong password, and the loop goes on to the
// next candidate.
func TestUnRARWithPasswords_PreVerifiedWrongPasswordsRunNoUnrar(t *testing.T) {
	t.Parallel()
	bin, logPath := recordingUnrar(t)
	archive := Archive{Type: RarArchive, MainFile: filepath.Join("testdata", "password_rar5.rar")}
	var lines []string
	opts := Options{
		UnrarCommand: bin,
		Passwords:    []string{"wrong1", "wrong2", "testpass"},
		OnLine:       func(l string) { lines = append(lines, l) },
	}
	res, err := UnRARWithPasswords(context.Background(), slog.Default(), archive, t.TempDir(), opts)
	if err != nil {
		t.Fatalf("UnRARWithPasswords: %v", err)
	}
	if res.Engine != "unrar" {
		t.Errorf("Engine = %q, want unrar", res.Engine)
	}
	got, readErr := os.ReadFile(logPath) //nolint:gosec // test path
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "-ptestpass\n" {
		t.Errorf("unrar ran with %q, want only -ptestpass", got)
	}
	skipped := 0
	for _, l := range lines {
		if strings.Contains(l, "Skipping known-wrong password") {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("OnLine reported %d pre-verified skips, want 2; lines %q", skipped, lines)
	}
}

// Every candidate proven wrong still ends in ErrWrongPassword, with unrar
// never run, and the Result names the engine.
func TestUnRARWithPasswords_AllPreVerifiedWrong(t *testing.T) {
	t.Parallel()
	bin, logPath := recordingUnrar(t)
	archive := Archive{Type: RarArchive, MainFile: filepath.Join("testdata", "password_rar5.rar")}
	opts := Options{UnrarCommand: bin, Passwords: []string{"wrong1", "wrong2"}}
	res, err := UnRARWithPasswords(context.Background(), slog.Default(), archive, t.TempDir(), opts)
	if !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("err = %v, want ErrWrongPassword", err)
	}
	if res.Engine != "unrar" || res.Reason != FailWrongPassword {
		t.Errorf("res = {Engine: %q, Reason: %v}, want unrar and FailWrongPassword", res.Engine, res.Reason)
	}
	if _, statErr := os.Stat(logPath); !os.IsNotExist(statErr) {
		t.Errorf("unrar ran (log stat err = %v), want no run", statErr)
	}
}

// TestRarPreVerify_ContentEncrypted proves rarPreVerify (which UnRAR
// consults before running unrar) correctly classifies a correct vs. wrong
// password against a real password-protected RAR5 fixture.
func TestRarPreVerify_ContentEncrypted(t *testing.T) {
	path := filepath.Join("testdata", "password_rar5.rar")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not present: %v", err)
	}
	archive := Archive{MainFile: path}

	if skip := rarPreVerify(archive, "testpass"); skip {
		t.Error("rarPreVerify() = true for the correct password, want false (do not skip)")
	}
	if skip := rarPreVerify(archive, "definitely-wrong"); !skip {
		t.Error("rarPreVerify() = false for a wrong password, want true (skip)")
	}
}

// TestRarPreVerify_FallsThroughWhenUncertain proves rarPreVerify never
// skips when it cannot positively prove a password wrong: a parse error
// (nonexistent file) and an archive with no encryption at all must both
// return skip=false so the real extraction attempt still runs.
func TestRarPreVerify_FallsThroughWhenUncertain(t *testing.T) {
	t.Run("unreadable archive", func(t *testing.T) {
		archive := Archive{MainFile: "/nonexistent/path/to/file.rar"}
		if skip := rarPreVerify(archive, "whatever"); skip {
			t.Error("rarPreVerify() = true for an unreadable archive, want false (fall through)")
		}
	})

	t.Run("unencrypted archive", func(t *testing.T) {
		path := filepath.Join("testdata", "single_rar5.rar")
		if _, err := os.Stat(path); err != nil {
			t.Skipf("fixture not present: %v", err)
		}
		archive := Archive{MainFile: path}
		if skip := rarPreVerify(archive, "anything"); skip {
			t.Error("rarPreVerify() = true for an unencrypted archive, want false (fall through)")
		}
	})
}
