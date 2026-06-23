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

func TestCleanupPartialFiles(t *testing.T) {
	tmp := t.TempDir()

	// Create initial file
	f1 := filepath.Join(tmp, "initial.txt")
	if err := os.WriteFile(f1, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}

	beforeSnap, err := snapshotDir(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Create partial files
	f2 := filepath.Join(tmp, "partial1.txt")
	if err := os.WriteFile(f2, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	f3 := filepath.Join(tmp, "subdir", "partial2.txt")
	if err := os.MkdirAll(filepath.Dir(f3), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f3, []byte("partial2"), 0600); err != nil {
		t.Fatal(err)
	}

	// Call cleanupPartialFiles
	cleanupPartialFiles(tmp, beforeSnap, slog.Default(), "test", 1)

	// Verify f1 still exists, but f2 and f3 are deleted
	if _, err := os.Stat(f1); err != nil {
		t.Errorf("initial file was deleted: %v", err)
	}
	if _, err := os.Stat(f2); err == nil {
		t.Errorf("partial1.txt was not cleaned up")
	}
	if _, err := os.Stat(f3); err == nil {
		t.Errorf("subdir/partial2.txt was not cleaned up")
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
				// Simulate creating a partial file to verify cleanup is triggered
				pfile := filepath.Join(outDir, "partial.txt")
				_ = os.WriteFile(pfile, []byte("partial"), 0600)

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

		// Verify partial file was cleaned up
		pfile := filepath.Join(outDir, "partial.txt")
		if _, err := os.Stat(pfile); err == nil {
			t.Error("partial.txt should have been cleaned up")
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
