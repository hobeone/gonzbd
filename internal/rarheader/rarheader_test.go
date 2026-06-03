package rarheader

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
