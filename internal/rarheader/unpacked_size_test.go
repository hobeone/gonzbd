package rarheader

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func unpackFixture(name string) string {
	return filepath.Join("..", "unpack", "testdata", name)
}

// The sizes are the ones `unrar lt` reports for each fixture's members.
func TestUnpackedSize(t *testing.T) {
	t.Parallel()
	multi := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		multi = append(multi, unpackFixture(fmt.Sprintf("multi_new.part%02d.rar", i)))
	}
	split := make([]string, 0, 6)
	for i := 1; i <= 6; i++ {
		split = append(split, unpackFixture(fmt.Sprintf("two_members.part%d.rar", i)))
	}
	tests := []struct {
		name    string
		volumes []string
		want    int64
	}{
		{"RAR5, files in a subdirectory", []string{unpackFixture("single_rar5.rar")}, 21 + 21 + 20},
		{"RAR5 with directory members", []string{unpackFixture("with_dirs.rar")}, 20},
		{"RAR5, compressed", []string{unpackFixture("compressed_rar5.rar")}, 336000},
		{"RAR5, content encrypted, headers plain", []string{unpackFixture("password_rar5.rar")}, 21},
		{"RAR5, one member over ten volumes", multi, 8192},
		{"RAR5, a member continued over six volumes", split, 35 + 4800},
		{"RAR3 with directory members", []string{filepath.Join("testdata", "rar3-subdirs.rar")}, 6 + 8 + 5 + 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := UnpackedSize(tc.volumes)
			if err != nil {
				t.Fatalf("UnpackedSize: %v", err)
			}
			if got != tc.want {
				t.Errorf("UnpackedSize = %d, want %d", got, tc.want)
			}
		})
	}
}

// rar3File is one RAR3 file header with packed bytes of data after it.
type rar3File struct {
	flags            uint16
	packed, unpacked uint32
}

// writeRAR3Volume writes a RAR 1.5-4.x volume holding files, built from the
// block layout in technote.txt. Its CRCs are zero: UnpackedSize reads none.
func writeRAR3Volume(t *testing.T, path string, files ...rar3File) {
	t.Helper()
	b := []byte("Rar!\x1a\x07\x00")
	b = append(b, 0, 0, rar3TypeMain, 0, 0, 13, 0, 0, 0, 0, 0, 0, 0)
	for _, f := range files {
		const name = "m"
		size := rar3BaseHeader + rar3FileFixed + len(name)
		h := []byte{0, 0, rar3TypeFile}
		h = binary.LittleEndian.AppendUint16(h, f.flags|rar3LongBlock)
		h = binary.LittleEndian.AppendUint16(h, uint16(size))
		h = binary.LittleEndian.AppendUint32(h, f.packed)
		h = binary.LittleEndian.AppendUint32(h, f.unpacked)
		h = append(h, make([]byte, 1+4+4+1+1)...) // HOST_OS, FILE_CRC, FTIME, UNP_VER, METHOD
		h = binary.LittleEndian.AppendUint16(h, uint16(len(name)))
		h = append(h, make([]byte, 4)...) // ATTR
		h = append(h, name...)
		b = append(b, h...)
		b = append(b, make([]byte, f.packed)...)
	}
	b = append(b, 0, 0, rar3TypeEnd, 0, 0, 7, 0)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A RAR3 member continued into a later volume is counted once, and a
// directory not at all, whatever size its header claims.
func TestUnpackedSize_RAR3Volumes(t *testing.T) {
	t.Parallel()
	const splitAfter = 0x0002
	dir := t.TempDir()
	v1, v2 := filepath.Join(dir, "set.rar"), filepath.Join(dir, "set.r00")
	writeRAR3Volume(t, v1,
		rar3File{flags: rar3FileDirMask, unpacked: 4096},
		rar3File{flags: splitAfter, packed: 3, unpacked: 1000},
	)
	writeRAR3Volume(t, v2,
		rar3File{flags: rar3FileSplitBefore, packed: 2, unpacked: 1000},
		rar3File{packed: 1, unpacked: 7},
	)
	got, err := UnpackedSize([]string{v1, v2})
	if err != nil {
		t.Fatalf("UnpackedSize: %v", err)
	}
	if got != 1000+7 {
		t.Errorf("UnpackedSize = %d, want %d", got, 1000+7)
	}

	// A main header marking the headers encrypted (MHD_PASSWORD) hides the
	// sizes that follow it.
	b, err := os.ReadFile(v1) //nolint:gosec // test fixture under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	b[len("Rar!\x1a\x07\x00")+3] |= rar3MainPassword
	if err := os.WriteFile(v1, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := UnpackedSize([]string{v1, v2}); !errors.Is(err, ErrSizeUnknown) {
		t.Errorf("UnpackedSize(encrypted RAR3 headers) err = %v, want ErrSizeUnknown", err)
	}
}

func TestUnpackedSize_NotDeclared(t *testing.T) {
	t.Parallel()
	notRAR := filepath.Join(t.TempDir(), "x.rar")
	if err := os.WriteFile(notRAR, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{unpackFixture("encrypted_header.rar"), notRAR} {
		if _, err := UnpackedSize([]string{v}); !errors.Is(err, ErrSizeUnknown) {
			t.Errorf("UnpackedSize(%s) err = %v, want ErrSizeUnknown", filepath.Base(v), err)
		}
	}
	if _, err := UnpackedSize([]string{filepath.Join(t.TempDir(), "missing.rar")}); err == nil || errors.Is(err, ErrSizeUnknown) {
		t.Errorf("UnpackedSize(missing) err = %v, want the open error", err)
	}
}
