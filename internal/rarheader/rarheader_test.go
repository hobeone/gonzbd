package rarheader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hobeone/rarengine"
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

func TestInspectRar3_FileNotFound(t *testing.T) {
	_, err := InspectRar3("/nonexistent/path/to/file.rar")
	if err == nil {
		t.Error("InspectRar3 returned nil error for nonexistent file")
	}
}

func TestInspectRar3_Corrupted(t *testing.T) {
	data := append([]byte{}, rar3Sig...)
	data = append(data, bytes.Repeat([]byte{0xFF}, 1000)...)
	path := writeTemp(t, "corrupt.rar", data)
	_, err := InspectRar3(path)
	if err == nil {
		t.Error("expected error for corrupted RAR3 file in InspectRar3")
	}
}

// TestInspectRar3_RealFixtures exercises InspectRar3 against real (not
// synthetic) RAR3 archives vendored from markokr/rarfile -- see
// testdata/ATTRIBUTION.md. rar3-comment-plain.rar uses a genuinely
// compressed method (3) plus archive- and file-level comment sub-blocks
// (type 0x7a, exercised via the default/skip case -- confirmed by
// instrumented run, not assumed); rar3-subdirs.rar uses the store method
// (0) with subdirectories and a Unicode filename. Both prove header-only
// inspection works regardless of compression method -- the gap
// InspectRar5's StreamDecompressor-based approach would hit for RAR3 (see
// https://github.com/hobeone/rarengine/issues/14).
func TestInspectRar3_RealFixtures(t *testing.T) {
	tests := []struct {
		name      string
		file      string
		wantFiles []string
	}{
		{
			name:      "compressed, archive and file comments",
			file:      filepath.Join("testdata", "rar3-comment-plain.rar"),
			wantFiles: []string{"file1.txt", "file2.txt"},
		},
		{
			// The third entry's on-disk name is "sub/üȵĩöḋè/file.txt", but
			// its header carries the RAR3 UNICODE flag: the raw name field
			// is an ASCII fallback name, a NUL separator, then RAR3's own
			// compact Unicode encoding (not raw UTF-8). rarengine's
			// ParseRAR3FileHeader does not decode that encoding -- it
			// returns the raw dual-encoded bytes as-is, which sanitizeName
			// then mangles into "_file_.txt" (NUL -> "_", then path.Base).
			// This is a pre-existing rarengine limitation, not something
			// introduced here; asserting the real (if ugly) output keeps
			// this test honest about current behavior.
			name:      "store method, subdirs, unicode name",
			file:      filepath.Join("testdata", "rar3-subdirs.rar"),
			wantFiles: []string{"file2.txt", "long fn.txt", "_file_.txt", "file1.txt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := InspectRar3(tt.file)
			if err != nil {
				t.Fatalf("InspectRar3(%s) error: %v", tt.file, err)
			}
			if info.Version != 3 {
				t.Errorf("Version = %d, want 3", info.Version)
			}
			if !slices.Equal(info.Filenames, tt.wantFiles) {
				t.Errorf("Filenames = %v, want %v", info.Filenames, tt.wantFiles)
			}
			if info.Encrypted {
				t.Error("Encrypted = true, want false")
			}
		})
	}
}

// TestInspect_RAR3Fixtures_NativePath proves Inspect() resolves these real
// RAR3 archives via InspectRar3 without falling back to unrar: execCommand
// is stubbed to fail immediately, so if Inspect fell back it would return
// that failure instead of a successful result.
func TestInspect_RAR3Fixtures_NativePath(t *testing.T) {
	oldExecCommand := execCommand
	defer func() { execCommand = oldExecCommand }()
	execCommand = func(name string, args ...string) *exec.Cmd {
		return exec.Command("false")
	}

	tests := []struct {
		file      string
		wantFiles []string
	}{
		{filepath.Join("testdata", "rar3-comment-plain.rar"), []string{"file1.txt", "file2.txt"}},
		{filepath.Join("testdata", "rar3-subdirs.rar"), []string{"file2.txt", "long fn.txt", "_file_.txt", "file1.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			info, err := Inspect(tt.file)
			if err != nil {
				t.Fatalf("Inspect(%s) error: %v (should have used InspectRar3, not fallen back to stubbed unrar)", tt.file, err)
			}
			if !slices.Equal(info.Filenames, tt.wantFiles) {
				t.Errorf("Filenames = %v, want %v", info.Filenames, tt.wantFiles)
			}
		})
	}
}

// buildRAR3FilePayload constructs the fixed-layout portion of a RAR3
// FILE_HEAD payload (per rarengine's ParseRAR3FileHeader: unpSize, hostOS,
// fileCrc, fTime, unpVer, method, nameSize, attr) followed by name and,
// when salt is true, an 8-byte salt/encryption marker.
func buildRAR3FilePayload(name string, salt bool) []byte {
	var u4 [4]byte
	payload := make([]byte, 0, 21+len(name)+8)
	payload = append(payload, u4[:]...) // unpSize = 0
	payload = append(payload, 0)        // hostOS
	payload = append(payload, u4[:]...) // fileCrc = 0
	payload = append(payload, u4[:]...) // fTime = 0
	payload = append(payload, 0)        // unpVer (unused)
	payload = append(payload, 0x30)     // method = store
	var u2 [2]byte
	binary.LittleEndian.PutUint16(u2[:], uint16(len(name)))
	payload = append(payload, u2[:]...) // nameSize
	payload = append(payload, u4[:]...) // attr = 0
	payload = append(payload, []byte(name)...)
	if salt {
		payload = append(payload, make([]byte, 8)...)
	}
	return payload
}

// buildRAR3FileHeaderBlock constructs a raw, CRC-valid RAR3 block header
// (as read by rarengine.ReadRAR3BlockHeader) of type FILE_HEAD (0x74),
// always with the LONG_BLOCK flag set (0x8000) since FILE_HEAD always
// carries an addSize field. addSize is the header's *declared* packed
// size, independent of how many payload bytes the caller actually
// appends after this block -- used to synthesize a truncated-data error.
func buildRAR3FileHeaderBlock(flags uint16, addSize uint32, payload []byte) []byte {
	const longBlockFlag = 0x8000
	blockFlags := flags | longBlockFlag
	hSize := uint16(11 + len(payload))

	var typeFlagsSize [5]byte
	typeFlagsSize[0] = rar3HeaderTypeFile
	binary.LittleEndian.PutUint16(typeFlagsSize[1:3], blockFlags)
	binary.LittleEndian.PutUint16(typeFlagsSize[3:5], hSize)

	var addSizeBuf [4]byte
	binary.LittleEndian.PutUint32(addSizeBuf[:], addSize)

	crcBuf := append(append([]byte{}, typeFlagsSize[:]...), addSizeBuf[:]...)
	crcBuf = append(crcBuf, payload...)
	crc := uint16(crc32.ChecksumIEEE(crcBuf))

	var out bytes.Buffer
	var crcField [2]byte
	binary.LittleEndian.PutUint16(crcField[:], crc)
	out.Write(crcField[:])
	out.Write(typeFlagsSize[:])
	out.Write(addSizeBuf[:])
	out.Write(payload)
	return out.Bytes()
}

// TestInspectRar3_Encrypted proves InspectRar3 sets Info.Encrypted from a
// file header's salt/password flag (0x0400) -- the one property claim in
// InspectRar3 that neither real fixture exercises (both are unencrypted).
func TestInspectRar3_Encrypted(t *testing.T) {
	const encryptedFlag = 0x0400
	payload := buildRAR3FilePayload("secret.txt", true)
	block := buildRAR3FileHeaderBlock(encryptedFlag, 0, payload)
	data := append(append([]byte{}, rar3Sig...), block...)
	path := writeTemp(t, "encrypted.rar", data)

	info, err := InspectRar3(path)
	if err != nil {
		t.Fatalf("InspectRar3(%s) error: %v", path, err)
	}
	if !info.Encrypted {
		t.Error("Encrypted = false, want true")
	}
	if len(info.Filenames) != 1 || info.Filenames[0] != "secret.txt" {
		t.Errorf("Filenames = %v, want [secret.txt]", info.Filenames)
	}
}

// TestInspectRar3_NegativePackedSize proves InspectRar3 rejects a file
// header whose HIGH_PACK extension (attacker-controlled) sets PackedSize's
// sign bit, rather than silently no-oping the packed-data skip (io.CopyN
// and skipForward both treat N<=0 as "nothing to do") and desyncing the
// stream to read attacker-chosen bytes as the next header.
func TestInspectRar3_NegativePackedSize(t *testing.T) {
	const highPackFlag = 0x0100
	name := "file.txt"

	payloadCap := 4 + 1 + 4 + 4 + 1 + 1 + 2 + 4 + 4 + 4 + len(name)
	payload := make([]byte, 0, payloadCap)
	var u4 [4]byte
	payload = append(payload, u4[:]...) // unpSize = 0
	payload = append(payload, 0)        // hostOS
	payload = append(payload, u4[:]...) // fileCrc = 0
	payload = append(payload, u4[:]...) // fTime = 0
	payload = append(payload, 0)        // unpVer (unused)
	payload = append(payload, 0x30)     // method = store
	var u2 [2]byte
	binary.LittleEndian.PutUint16(u2[:], uint16(len(name)))
	payload = append(payload, u2[:]...) // nameSize
	payload = append(payload, u4[:]...) // attr = 0

	var highPack [4]byte
	binary.LittleEndian.PutUint32(highPack[:], 0x80000000) // sign bit set
	payload = append(payload, highPack[:]...)              // highPack
	payload = append(payload, u4[:]...)                    // highUnp = 0
	payload = append(payload, []byte(name)...)

	block := buildRAR3FileHeaderBlock(highPackFlag, 0, payload) // addSize=0, so PackedSize = 0 | (highPack<<32) < 0
	data := append(append([]byte{}, rar3Sig...), block...)
	path := writeTemp(t, "negsize.rar", data)

	_, err := InspectRar3(path)
	if err == nil {
		t.Fatal("expected error for negative packed size, got nil")
	}
	if !strings.Contains(err.Error(), "negative packed size") {
		t.Errorf("error = %v, want it to mention 'negative packed size'", err)
	}
}

// TestInspectRar3_TruncatedPackedData proves InspectRar3 surfaces an error
// (rather than silently truncating or hanging) when a file header declares
// more packed data than the stream actually contains.
func TestInspectRar3_TruncatedPackedData(t *testing.T) {
	payload := buildRAR3FilePayload("file.txt", false)
	block := buildRAR3FileHeaderBlock(0, 1000, payload) // claims 1000 packed bytes that don't exist
	data := append(append([]byte{}, rar3Sig...), block...)
	path := writeTemp(t, "truncated.rar", data)

	_, err := InspectRar3(path)
	if err == nil {
		t.Fatal("expected error for truncated packed data, got nil")
	}
	if !strings.Contains(err.Error(), "skip packed data") {
		t.Errorf("error = %v, want it to mention 'skip packed data'", err)
	}
}

func TestSkipForward(t *testing.T) {
	tests := []struct {
		name    string
		n       int64
		wantErr bool
	}{
		{"zero", 0, false},
		{"negative treated as no-op", -5, false},
		{"within bounds", 3, false},
		{"past EOF", 1000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, "skip.bin", []byte("hello world"))
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = f.Close() }()

			err = skipForward(f, tt.n)
			if (err != nil) != tt.wantErr {
				t.Errorf("skipForward(f, %d) err = %v, wantErr = %v", tt.n, err, tt.wantErr)
			}
		})
	}
}

func TestOpenPastSignature_FileNotFound(t *testing.T) {
	_, err := openPastSignature("/nonexistent/path/to/file.rar", rar5Sig)
	if err == nil {
		t.Error("openPastSignature returned nil error for nonexistent file")
	}
}

func TestOpenPastSignature_ShorterThanSignature(t *testing.T) {
	path := writeTemp(t, "short.rar", rar5Sig[:len(rar5Sig)-1])
	_, err := openPastSignature(path, rar5Sig)
	if err == nil {
		t.Error("openPastSignature returned nil error for file shorter than signature")
	}
	if !strings.Contains(err.Error(), "skip signature") {
		t.Errorf("error = %v, want it to mention 'skip signature'", err)
	}
}

// TestRecoverVolumeExtension_OpenPastSignatureFails exercises
// RecoverVolumeExtension's error path when openPastSignature's open call
// fails after readMagic already succeeded on the same path. In real usage
// this can only happen via a TOCTOU race (the file changing between
// readMagic's and openPastSignature's independent os.Open calls); osOpen
// is stubbed here to make that branch deterministically reachable instead
// of leaving it untested.
func TestRecoverVolumeExtension_OpenPastSignatureFails(t *testing.T) {
	path := writeTemp(t, "test.rar", rar5Sig) // readMagic succeeds: valid RAR5 signature

	oldOsOpen := osOpen
	defer func() { osOpen = oldOsOpen }()
	osOpen = func(string) (*os.File, error) {
		return nil, errors.New("injected open failure")
	}

	_, _, err := RecoverVolumeExtension(path)
	if err == nil {
		t.Fatal("expected error when openPastSignature's open fails, got nil")
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

func TestVerifyPassword_ContentEncrypted(t *testing.T) {
	path := filepath.Join("..", "unpack", "testdata", "password_rar5.rar")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not present: %v", err)
	}

	t.Run("correct password", func(t *testing.T) {
		verified, hasCheckValue, err := VerifyPassword(path, "testpass")
		if err != nil {
			t.Fatalf("VerifyPassword() unexpected error: %v", err)
		}
		if !hasCheckValue {
			t.Fatalf("VerifyPassword() hasCheckValue = false, want true")
		}
		if !verified {
			t.Errorf("VerifyPassword() verified = false, want true for correct password")
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		verified, hasCheckValue, err := VerifyPassword(path, "definitely-wrong")
		if err != nil {
			t.Fatalf("VerifyPassword() unexpected error: %v", err)
		}
		if !hasCheckValue {
			t.Fatalf("VerifyPassword() hasCheckValue = false, want true")
		}
		if verified {
			t.Errorf("VerifyPassword() verified = true, want false for wrong password")
		}
	})
}

func TestVerifyPassword_HeaderEncrypted(t *testing.T) {
	path := filepath.Join("..", "unpack", "testdata", "encrypted_header.rar")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not present: %v", err)
	}

	t.Run("correct password", func(t *testing.T) {
		verified, hasCheckValue, err := VerifyPassword(path, "testpass")
		if err != nil {
			t.Fatalf("VerifyPassword() unexpected error: %v", err)
		}
		if !hasCheckValue {
			t.Fatalf("VerifyPassword() hasCheckValue = false, want true")
		}
		if !verified {
			t.Errorf("VerifyPassword() verified = false, want true for correct password")
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		verified, hasCheckValue, err := VerifyPassword(path, "definitely-wrong")
		if err != nil {
			t.Fatalf("VerifyPassword() unexpected error: %v", err)
		}
		if !hasCheckValue {
			t.Fatalf("VerifyPassword() hasCheckValue = false, want true")
		}
		if verified {
			t.Errorf("VerifyPassword() verified = true, want false for wrong password")
		}
	})
}

func TestVerifyPassword_NotEncrypted(t *testing.T) {
	path := filepath.Join("..", "unpack", "testdata", "single_rar5.rar")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not present: %v", err)
	}

	verified, hasCheckValue, err := VerifyPassword(path, "anything")
	if err != nil {
		t.Fatalf("VerifyPassword() unexpected error: %v", err)
	}
	if hasCheckValue {
		t.Fatalf("VerifyPassword() hasCheckValue = true, want false for an unencrypted archive")
	}
	if verified {
		t.Fatalf("VerifyPassword() verified = true, want false for an unencrypted archive")
	}
}

func TestVerifyPassword_NotRAR5(t *testing.T) {
	path := writeTemp(t, "test.rar", rar3Sig)
	_, _, err := VerifyPassword(path, "anything")
	if err != nil {
		t.Fatalf("VerifyPassword() unexpected error for non-RAR5 archive: %v", err)
	}
}

func TestVerifyPassword_FileNotFound(t *testing.T) {
	_, _, err := VerifyPassword("/nonexistent/path/to/file.rar", "anything")
	if err == nil {
		t.Error("VerifyPassword() returned nil error for nonexistent file")
	}
}

func TestRecoverVolumeExtension_MultiVolumeRAR5(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		file          string
		wantVolumeIdx int
		wantMultiVol  bool
	}{
		{"first volume, no explicit flag", "../unpack/testdata/multi_new.part01.rar", 0, true},
		{"second volume", "../unpack/testdata/multi_new.part02.rar", 1, true},
		{"tenth volume", "../unpack/testdata/multi_new.part10.rar", 9, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idx, multiVol, err := RecoverVolumeExtension(tt.file)
			if err != nil {
				t.Fatalf("RecoverVolumeExtension(%s): %v", tt.file, err)
			}
			if idx != tt.wantVolumeIdx {
				t.Errorf("volumeIndex = %d; want %d", idx, tt.wantVolumeIdx)
			}
			if multiVol != tt.wantMultiVol {
				t.Errorf("multiVolume = %v; want %v", multiVol, tt.wantMultiVol)
			}
		})
	}
}

func TestRecoverVolumeExtension_NotRAR(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := dir + "/not-a-rar.txt"
	if err := os.WriteFile(p, []byte("plain text, not a RAR archive"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := RecoverVolumeExtension(p)
	if !errors.Is(err, ErrNotRAR) {
		t.Errorf("err = %v; want ErrNotRAR", err)
	}
}

func TestRecoverVolumeExtension_RAR3ReturnsError(t *testing.T) {
	t.Parallel()

	// encrypted_header.rar in testdata is RAR5; use a RAR3 fixture if one
	// exists in internal/unpack/testdata, otherwise construct a minimal
	// RAR3-signature-only file (RAR3 support is explicitly out of scope --
	// this test only needs to prove the function doesn't panic or silently
	// return a wrong answer for RAR3 input, not exercise real RAR3 parsing).
	dir := t.TempDir()
	p := dir + "/rar3.rar"
	if err := os.WriteFile(p, rar3Sig, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, _, err := RecoverVolumeExtension(p)
	if err == nil {
		t.Error("RecoverVolumeExtension on RAR3 signature: expected an error (RAR3 unsupported), got nil")
	}
}

// buildArchiveHeaderBlock constructs a raw, CRC-valid RAR5 main archive
// header block (HeaderTypeArchive) carrying the given archive-level flags
// and, when hasVolNum is true, an explicit volume-number field. Real RAR5
// archives never emit an explicit VolumeNumber of exactly 0 (the first
// volume always omits the flag entirely, relying on rarengine's -1
// sentinel; the second volume's explicit number is already 1) so this
// synthetic construction is the only way to exercise
// RecoverVolumeExtension's "< 0" boundary against a genuine VolumeNumber==0
// input and kill the CONDITIONALS_BOUNDARY mutant on that comparison.
func buildArchiveHeaderBlock(archiveFlags uint64, hasVolNum bool, volNum uint64) []byte {
	archivePayload := rarengine.EncodeVint(archiveFlags)
	if hasVolNum {
		archivePayload = append(archivePayload, rarengine.EncodeVint(volNum)...)
	}

	const blockFlags = 0 // no HasExtra, no HasData
	payload := append([]byte{}, rarengine.EncodeVint(rarengine.HeaderTypeArchive)...)
	payload = append(payload, rarengine.EncodeVint(blockFlags)...)
	payload = append(payload, archivePayload...)

	sizeV := rarengine.EncodeVint(uint64(len(payload)))
	buf := append(append([]byte{}, sizeV...), payload...)

	crc := crc32.ChecksumIEEE(buf)
	var out bytes.Buffer
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc)
	out.Write(crcBuf[:])
	out.Write(buf)
	return out.Bytes()
}

// TestRecoverVolumeExtension_ExplicitVolumeNumberZero proves the "first
// volume" normalization in RecoverVolumeExtension is keyed specifically on
// rarengine's -1 sentinel (no explicit flag), not on "volume number <= 0".
// A synthetic archive header with ArcFlagVolNum set and an explicit
// VolumeNumber of 0 must be returned as volumeIndex 0 via the ah.VolumeNumber
// return path, not misclassified by a boundary error.
func TestRecoverVolumeExtension_ExplicitVolumeNumberZero(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	buf.Write(rar5Sig)
	buf.Write(buildArchiveHeaderBlock(rarengine.ArcFlagMultiVol|rarengine.ArcFlagVolNum, true, 0))

	dir := t.TempDir()
	p := filepath.Join(dir, "synthetic.rar")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	idx, multiVol, err := RecoverVolumeExtension(p)
	if err != nil {
		t.Fatalf("RecoverVolumeExtension(%s): %v", p, err)
	}
	if !multiVol {
		t.Errorf("multiVolume = false; want true")
	}
	if idx != 0 {
		t.Errorf("volumeIndex = %d; want 0 (explicit VolumeNumber==0)", idx)
	}
}

// buildRAR5Block constructs a raw, CRC-valid RAR5 block header (as read by
// rarengine.ReadBlockHeader) of the given type/flags, with a HasData
// payload of dataSize bytes when flags carries HeaderFlagHasData.
func buildRAR5Block(headerType, flags, dataSize uint64) []byte {
	payload := append([]byte{}, rarengine.EncodeVint(headerType)...)
	payload = append(payload, rarengine.EncodeVint(flags)...)
	if flags&rarengine.HeaderFlagHasData > 0 {
		payload = append(payload, rarengine.EncodeVint(dataSize)...)
	}
	sizeV := rarengine.EncodeVint(uint64(len(payload)))
	buf := append(append([]byte{}, sizeV...), payload...)

	crc := crc32.ChecksumIEEE(buf)
	var out bytes.Buffer
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc)
	out.Write(crcBuf[:])
	out.Write(buf)
	return out.Bytes()
}

// TestVerifyPasswordFromHeaders_SkipsUnknownBlockData proves
// verifyPasswordFromHeaders's default case correctly skips over the data
// payload of a block type it doesn't otherwise special-case (anything
// other than HEAD_CRYPT/HEAD_FILE/HEAD_SERVICE/HEAD_ENDARC that carries a
// HasData payload) before continuing to scan for the real encryption/file
// information -- exercising the DataSize-skip path that real fixture
// archives never happen to hit (their archive headers carry no data).
func TestVerifyPasswordFromHeaders_SkipsUnknownBlockData(t *testing.T) {
	const arbitraryHeaderType = 6 // not archive/file/service/encryption/end
	var buf bytes.Buffer
	buf.Write(buildRAR5Block(arbitraryHeaderType, rarengine.HeaderFlagHasData, 4))
	buf.Write([]byte{0xAA, 0xBB, 0xCC, 0xDD}) // the 4 bytes of skipped data
	buf.Write(buildRAR5Block(rarengine.HeaderTypeEnd, 0, 0))

	verified, hasCheckValue, err := verifyPasswordFromHeaders(&buf, "irrelevant")
	if err != nil {
		t.Fatalf("verifyPasswordFromHeaders() unexpected error: %v", err)
	}
	if hasCheckValue {
		t.Errorf("hasCheckValue = true, want false (no encryption in this synthetic stream)")
	}
	if verified {
		t.Errorf("verified = true, want false")
	}
	if buf.Len() != 0 {
		t.Errorf("expected the whole synthetic stream to be consumed, %d bytes remain", buf.Len())
	}
}
