package rarheader

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsRAR_RAR3Signature(t *testing.T) {
	path := writeTemp(t, "test.rar", rar3Sig)
	ok, err := IsRAR(path)
	if err != nil {
		t.Fatalf("IsRAR(%s) error: %v", path, err)
	}
	if !ok {
		t.Error("IsRAR returned false for RAR3 signature")
	}
}

func TestIsRAR_RAR5Signature(t *testing.T) {
	path := writeTemp(t, "test.rar", rar5Sig)
	ok, err := IsRAR(path)
	if err != nil {
		t.Fatalf("IsRAR(%s) error: %v", path, err)
	}
	if !ok {
		t.Error("IsRAR returned false for RAR5 signature")
	}
}

func TestIsRAR_NotRAR(t *testing.T) {
	// MKV magic: 0x1A 0x45 0xDF 0xA3
	path := writeTemp(t, "video.mkv", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x00, 0x00, 0x00, 0x00})
	ok, err := IsRAR(path)
	if err != nil {
		t.Fatalf("IsRAR(%s) error: %v", path, err)
	}
	if ok {
		t.Error("IsRAR returned true for MKV file")
	}
}

func TestIsRAR_TooShort(t *testing.T) {
	path := writeTemp(t, "tiny", []byte{0x52, 0x61})
	ok, err := IsRAR(path)
	if err != nil {
		t.Fatalf("IsRAR(%s) error: %v", path, err)
	}
	if ok {
		t.Error("IsRAR returned true for file shorter than signature")
	}
}

func TestIsRAR_EmptyFile(t *testing.T) {
	path := writeTemp(t, "empty", []byte{})
	ok, err := IsRAR(path)
	if err != nil {
		t.Fatalf("IsRAR(%s) error: %v", path, err)
	}
	if ok {
		t.Error("IsRAR returned true for empty file")
	}
}

func TestIsRAR_RAR3WithTrailingData(t *testing.T) {
	// RAR3 signature followed by arbitrary data — should still be detected.
	data := append([]byte{}, rar3Sig...)
	data = append(data, 0xFF, 0xFE, 0xFD, 0xFC, 0x00, 0x00, 0x00, 0x00)
	path := writeTemp(t, "test.bin", data)
	ok, err := IsRAR(path)
	if err != nil {
		t.Fatalf("IsRAR(%s) error: %v", path, err)
	}
	if !ok {
		t.Error("IsRAR returned false for RAR3 signature with trailing data")
	}
}

func TestIsRAR_FileNotFound(t *testing.T) {
	_, err := IsRAR("/nonexistent/path/to/file.rar")
	if err == nil {
		t.Error("IsRAR returned nil error for nonexistent file")
	}
}

func TestDetectVersion_RAR3(t *testing.T) {
	data := append([]byte{}, rar3Sig...)
	data = append(data, 0x00) // extra byte
	path := writeTemp(t, "v3.rar", data)
	ver, err := readMagic(path)
	if err != nil {
		t.Fatalf("readMagic error: %v", err)
	}
	if ver != 3 {
		t.Errorf("readMagic = %d, want 3", ver)
	}
}

func TestDetectVersion_RAR5(t *testing.T) {
	data := append([]byte{}, rar5Sig...)
	data = append(data, 0x00) // extra byte
	path := writeTemp(t, "v5.rar", data)
	ver, err := readMagic(path)
	if err != nil {
		t.Fatalf("readMagic error: %v", err)
	}
	if ver != 5 {
		t.Errorf("readMagic = %d, want 5", ver)
	}
}

func TestDetectVersion_NotRAR(t *testing.T) {
	path := writeTemp(t, "text.txt", []byte("hello world"))
	_, err := readMagic(path)
	if !errors.Is(err, ErrNotRAR) {
		t.Errorf("readMagic error = %v, want ErrNotRAR", err)
	}
}

func TestInspect_NotRAR(t *testing.T) {
	path := writeTemp(t, "notrar.bin", []byte("this is not a RAR file at all, just plain text"))
	_, err := Inspect(path)
	if !errors.Is(err, ErrNotRAR) {
		t.Errorf("Inspect error = %v, want ErrNotRAR", err)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "movie.mkv", "movie.mkv"},
		{"unix_traversal", "../../etc/passwd", "passwd"},
		{"windows_traversal", `..\..\etc\passwd`, "passwd"},
		{"deep_traversal", "../../../../../../../tmp/evil.sh", "evil.sh"},
		{"absolute_unix", "/etc/shadow", "shadow"},
		{"absolute_windows", `C:\Windows\System32\cmd.exe`, "cmd.exe"},
		{"mixed_separators", `foo/bar\baz/qux.txt`, "qux.txt"},
		{"null_bytes", "file\x00name.txt", "file_name.txt"},
		{"empty", "", "unknown"},
		{"dot", ".", "unknown"},
		{"slash", "/", "unknown"},
		{"just_slashes", "///", "unknown"},
		{"backslash_only", `\`, "unknown"},
		{"current_dir_prefix", "./file.txt", "file.txt"},
		{"dotdot", "..", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeName(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// writeTemp creates a temporary file with the given content and returns its path.
// The file is cleaned up when the test finishes.
func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("writeTemp: %v", err)
	}
	return path
}

func TestIsPasswordError(t *testing.T) {
	// Subprocess helper to get an exit status 11 error.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	exit11Err := cmd.Run()
	if exit11Err == nil {
		t.Fatal("expected helper process to fail with exit status 11")
	}

	cases := []struct {
		name      string
		err       error
		stdout    string
		stderr    string
		wantRetry bool
	}{
		{"no error", nil, "", "", false},
		{"incorrect password stdout", nil, "Incorrect password", "", true},
		{"incorrect password stderr", nil, "", "Incorrect password", true},
		{"password stdout", nil, "password required", "", true},
		{"password stderr", nil, "", "password required", true},
		{"exit code 11", exit11Err, "", "", true},
		{"generic error", errors.New("unrar failed"), "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isPasswordError(tc.err, tc.stdout, tc.stderr)
			if got != tc.wantRetry {
				t.Errorf("isPasswordError(%v, %q, %q) = %v, want %v", tc.err, tc.stdout, tc.stderr, got, tc.wantRetry)
			}
		})
	}
}

func TestParseUnrarVtOutput(t *testing.T) {
	output := `
Name: file1.txt
Type: File
Flags: encrypted
Name: file2.txt
Type: File
Flags: directory
Name: path/to/file3.txt
Type: File
Flags: encrypted, solid
`
	filenames, encrypted := parseUnrarVtOutput(output)
	if !encrypted {
		t.Error("parseUnrarVtOutput did not detect encrypted flag")
	}
	want := []string{"file1.txt", "file2.txt", "file3.txt"}
	if len(filenames) != len(want) {
		t.Fatalf("got %d filenames, want %d", len(filenames), len(want))
	}
	for i, f := range filenames {
		if f != want[i] {
			t.Errorf("filenames[%d] = %q, want %q", i, f, want[i])
		}
	}
}

// Helper process for TestIsPasswordError to produce exit status 11.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(11)
}

func TestIsRAR_FileNotFound_CheckBool(t *testing.T) {
	ok, err := IsRAR("/nonexistent/path/to/file.rar")
	if err == nil {
		t.Error("IsRAR returned nil error for nonexistent file")
	}
	if ok {
		t.Error("IsRAR returned true for nonexistent file")
	}
}

func TestInspectRar5_FileNotFound(t *testing.T) {
	_, err := InspectRar5("/nonexistent/path/to/file.rar")
	if err == nil {
		t.Error("InspectRar5 returned nil error for nonexistent file")
	}
}

func TestInspectRar5_Corrupted(t *testing.T) {
	data := append([]byte{}, rar5Sig...)
	data = append(data, bytes.Repeat([]byte{0xFF}, 1000)...)
	path := writeTemp(t, "corrupt.rar", data)
	_, err := InspectRar5(path)
	if err == nil {
		t.Error("expected error for corrupted RAR5 file in InspectRar5")
	}
}

func TestInspect_CorruptedRAR5_Fallback(t *testing.T) {
	data := append([]byte{}, rar5Sig...)
	data = append(data, bytes.Repeat([]byte{0xFF}, 1000)...)
	path := writeTemp(t, "corrupt.rar", data)
	_, err := Inspect(path)
	if err == nil {
		t.Error("expected error for corrupted RAR5 file in Inspect")
	}
}

func TestInspect_ValidRAR_Fixtures(t *testing.T) {
	// Test on sample.rar
	path5 := filepath.Join("..", "..", "test", "fixtures", "rar", "sample.rar")
	if _, err := os.Stat(path5); err == nil {
		info, err := Inspect(path5)
		if err != nil {
			t.Fatalf("Inspect(%s) error: %v", path5, err)
		}
		if info.Version != 5 && info.Version != 3 {
			t.Errorf("unexpected version: %d", info.Version)
		}
		if len(info.Filenames) == 0 {
			t.Errorf("expected filenames in sample.rar, got none")
		}
	}

	// Test on multi-volume segment
	pathPart := filepath.Join("..", "..", "test", "fixtures", "rar", "multivolume", "du_test.part1.rar")
	if _, err := os.Stat(pathPart); err == nil {
		// Test Inspect (fallback or pure Go)
		info, err := Inspect(pathPart)
		if err != nil {
			t.Fatalf("Inspect(%s) error: %v", pathPart, err)
		}
		if info.Version != 5 {
			t.Errorf("expected version 5, got %d", info.Version)
		}
		if len(info.Filenames) == 0 {
			t.Errorf("expected filenames in du_test.part1.rar, got none")
		}

		// Directly test InspectRar5 to bypass Inspect fallback and kill mutants
		infoRar5, err := InspectRar5(pathPart)
		if err != nil {
			t.Fatalf("InspectRar5(%s) error: %v", pathPart, err)
		}
		if len(infoRar5.Filenames) == 0 {
			t.Errorf("expected filenames in InspectRar5(du_test.part1.rar), got none")
		}
	}
}

func TestInspectViaUnrar_Success(t *testing.T) {
	oldExecCommand := execCommand
	defer func() { execCommand = oldExecCommand }()

	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess_UnrarSuccess")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}

	info, err := inspectViaUnrar("dummy.rar", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !info.Encrypted {
		t.Error("expected encrypted=true")
	}
	if len(info.Filenames) != 1 || info.Filenames[0] != "movie.mkv" {
		t.Errorf("unexpected filenames: %v", info.Filenames)
	}
}

func TestInspectViaUnrar_Failure(t *testing.T) {
	oldExecCommand := execCommand
	defer func() { execCommand = oldExecCommand }()

	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess_UnrarFailure")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}

	_, err := inspectViaUnrar("dummy.rar", 3)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unrar vt failed") {
		t.Errorf("expected unrar vt failed error, got: %v", err)
	}
}

func TestInspectViaUnrar_PasswordError(t *testing.T) {
	oldExecCommand := execCommand
	defer func() { execCommand = oldExecCommand }()

	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess_UnrarPasswordError")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}

	info, err := inspectViaUnrar("dummy.rar", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !info.Encrypted {
		t.Error("expected encrypted=true")
	}
	if !info.HeaderEncrypted {
		t.Error("expected headerEncrypted=true")
	}
}

func TestHelperProcess_UnrarPasswordError(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Stdout.WriteString("Access denied: incorrect password?\n")
	os.Exit(11)
}

func TestHelperProcess_UnrarSuccess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	// Output valid unrar vt output
	os.Stdout.WriteString("Name: movie.mkv\nFlags: encrypted\n")
	os.Exit(0)
}

func TestHelperProcess_UnrarFailure(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	os.Stderr.WriteString("some unrar error")
	os.Exit(1)
}

func TestIsRARReader(t *testing.T) {
	t.Parallel()

	t.Run("RAR5 reader", func(t *testing.T) {
		r := bytes.NewReader(rar5Sig)
		ok, err := IsRARReader(r)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Error("expected true for RAR5 signature")
		}
	})

	t.Run("RAR3 reader", func(t *testing.T) {
		r := bytes.NewReader(rar3Sig)
		ok, err := IsRARReader(r)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Error("expected true for RAR3 signature")
		}
	})

	t.Run("non-RAR reader", func(t *testing.T) {
		r := bytes.NewReader([]byte("not a rar file"))
		ok, err := IsRARReader(r)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ok {
			t.Error("expected false for non-RAR signature")
		}
	})
}
