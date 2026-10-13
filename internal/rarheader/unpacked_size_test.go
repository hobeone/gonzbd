package rarheader

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

// rar3File is one RAR3 file header with packed bytes of data after it. typ
// defaults to a file header; highUnpacked, with rar3FileLarge in flags, is
// the high 32 bits of the unpacked size. raw, when set, is written in place
// of a file header.
type rar3File struct {
	typ              byte
	flags            uint16
	packed, unpacked uint32
	highUnpacked     uint32
	raw              []byte
}

// rar3VolumeBytes returns a RAR 1.5-4.x volume holding files, built from the
// block layout in technote.txt, ending in an end-of-archive block when end is
// set. Its CRCs are zero: UnpackedSize reads none.
func rar3VolumeBytes(end bool, files ...rar3File) []byte {
	b := []byte("Rar!\x1a\x07\x00")
	b = append(b, 0, 0, rar3TypeMain, 0, 0, 13, 0, 0, 0, 0, 0, 0, 0)
	for _, f := range files {
		if f.raw != nil {
			b = append(b, f.raw...)
			continue
		}
		typ := cmp.Or(f.typ, rar3TypeFile)
		const name = "m"
		size := rar3BaseHeader + rar3FileFixed + len(name)
		if f.flags&rar3FileLarge != 0 {
			size += 8
		}
		h := []byte{0, 0, typ}
		h = binary.LittleEndian.AppendUint16(h, f.flags|rar3LongBlock)
		h = binary.LittleEndian.AppendUint16(h, uint16(size)) //nolint:gosec // a few dozen bytes
		h = binary.LittleEndian.AppendUint32(h, f.packed)
		h = binary.LittleEndian.AppendUint32(h, f.unpacked)
		h = append(h, make([]byte, 1+4+4+1+1)...) // HOST_OS, FILE_CRC, FTIME, UNP_VER, METHOD
		h = binary.LittleEndian.AppendUint16(h, uint16(len(name)))
		h = append(h, make([]byte, 4)...) // ATTR
		if f.flags&rar3FileLarge != 0 {
			h = append(h, make([]byte, 4)...) // HIGH_PACK_SIZE
			h = binary.LittleEndian.AppendUint32(h, f.highUnpacked)
		}
		h = append(h, name...)
		b = append(b, h...)
		b = append(b, make([]byte, f.packed)...)
	}
	if end {
		b = append(b, 0, 0, rar3TypeEnd, 0, 0, 7, 0)
	}
	return b
}

// writeRAR3Volume writes rar3VolumeBytes(true, files...) to path.
func writeRAR3Volume(t *testing.T, path string, files ...rar3File) {
	t.Helper()
	if err := os.WriteFile(path, rar3VolumeBytes(true, files...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rar3UnpackedSize adds a large file's high size bits, counts no service
// (sub) block, steps over another block's ADD_SIZE data, and ends at the end
// of the volume when it has no end-of-archive block.
func TestRar3UnpackedSize_BlockKinds(t *testing.T) {
	t.Parallel()
	// An old-style comment block (0x75) carrying 5 bytes of data.
	other := []byte{0, 0, 0x75}
	other = binary.LittleEndian.AppendUint16(other, rar3LongBlock)
	other = binary.LittleEndian.AppendUint16(other, 11)
	other = binary.LittleEndian.AppendUint32(other, 5)
	other = append(other, make([]byte, 5)...)
	b := rar3VolumeBytes(false,
		rar3File{flags: rar3FileLarge, packed: 1, unpacked: 3, highUnpacked: 1},
		rar3File{typ: rar3TypeSub, packed: 2, unpacked: 999},
		rar3File{raw: other},
		rar3File{packed: 1, unpacked: 4},
	)
	got, err := rar3UnpackedSize(openRAR(t, b))
	if err != nil {
		t.Fatalf("rar3UnpackedSize: %v", err)
	}
	if want := int64(1)<<32 + 3 + 4; got != want {
		t.Errorf("rar3UnpackedSize = %d, want %d", got, want)
	}
}

// rar5FileSize reads a file header's size, and reports one declared
// unknown, a directory as nothing, and a size out of range or cut short as
// errors.
func TestRar5FileSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    []byte
		want    int64
		wantErr error
	}{
		{"plain", []byte{0x00, 0x2a}, 42, nil},
		{"directory", []byte{byte(rar5FileDir), 0x2a}, 0, nil},
		{"size unknown", []byte{byte(rar5FileSizeUnknown), 0x2a}, 0, ErrSizeUnknown},
	}
	for _, c := range cases {
		got, err := rar5FileSize(c.body)
		if got != c.want || !errors.Is(err, c.wantErr) {
			t.Errorf("%s: rar5FileSize = %d, %v; want %d, %v", c.name, got, err, c.want, c.wantErr)
		}
	}
	huge := append([]byte{0x00}, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x41) // (1<<62) + (1<<56)
	for name, body := range map[string][]byte{"out of range": huge, "no size": {0x00}, "empty": nil} {
		if _, err := rar5FileSize(body); err == nil || errors.Is(err, ErrSizeUnknown) {
			t.Errorf("%s: rar5FileSize err = %v, want a damage error", name, err)
		}
	}
	if _, _, err := readVint(&byteSliceReader{b: bytes.Repeat([]byte{0x80}, 11)}); err == nil {
		t.Error("readVint(11 continuation bytes) err = nil")
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

// openRAR writes b to a file and opens it for the walkers, which read from
// just past the signature.
func openRAR(t *testing.T, b []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.rar")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p) //nolint:gosec // test fixture under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// A damaged block header is an error, not a size: a declared header size of
// zero, or a header running past the end of the volume. A volume that ends
// where a block would start has no more blocks.
func TestRar5Block_DamagedHeader(t *testing.T) {
	t.Parallel()
	sig := []byte("Rar!\x1a\x07\x01\x00")
	off := int64(len(sig))
	cases := map[string][]byte{
		"zero header size":        append(append([]byte(nil), sig...), 0, 0, 0, 0, 0x00),
		"header past end of file": append(append([]byte(nil), sig...), 0, 0, 0, 0, 0x20, 0x02),
	}
	for name, b := range cases {
		if _, _, err := rar5Block(openRAR(t, b), off); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("%s: rar5Block err = %v, want a damage error", name, err)
		}
	}
	if _, _, err := rar5Block(openRAR(t, sig), off); !errors.Is(err, io.EOF) {
		t.Errorf("rar5Block at end of volume err = %v, want io.EOF", err)
	}

	// A data size past 1<<62 is damage.
	huge := append(append([]byte(nil), sig...), 0, 0, 0, 0, 11, 2, byte(rar5FlagData),
		0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x41)
	if _, _, err := rar5Block(openRAR(t, huge), off); err == nil {
		t.Error("rar5Block(data size out of range) err = nil")
	}

	// With an extra area and data, the body follows both sizes and the next
	// block follows the data.
	withData := append(append([]byte(nil), sig...), 0, 0, 0, 0, 5, 3, byte(rar5FlagExtra|rar5FlagData), 0, 5, 0x7e)
	hdr, next, err := rar5Block(openRAR(t, withData), off)
	if err != nil {
		t.Fatalf("rar5Block(extra and data): %v", err)
	}
	if hdr.typ != 3 || !bytes.Equal(hdr.body, []byte{0x7e}) || next != off+4+1+5+5 {
		t.Errorf("rar5Block(extra and data) = %+v, next %d; want type 3, body [7e], next %d", hdr, next, off+4+1+5+5)
	}
}

// rar5UnpackedSize reads a volume from just past its signature and reports
// a damaged block rather than counting past it.
func TestRar5UnpackedSize_Direct(t *testing.T) {
	t.Parallel()
	f, err := os.Open(unpackFixture("single_rar5.rar"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	if got, err := rar5UnpackedSize(f); err != nil || got != 62 {
		t.Errorf("rar5UnpackedSize = %d, %v; want 62, nil", got, err)
	}
	damaged := append([]byte("Rar!\x1a\x07\x01\x00"), 0, 0, 0, 0, 0x00)
	if _, err := rar5UnpackedSize(openRAR(t, damaged)); err == nil {
		t.Error("rar5UnpackedSize(damaged block) err = nil")
	}
}

// rar3UnpackedSize reports a file header cut short of its fixed fields, and
// a header size below the base header, rather than counting past them.
func TestRar3UnpackedSize_Direct(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "set.rar")
	writeRAR3Volume(t, p, rar3File{packed: 4, unpacked: 9})
	b, err := os.ReadFile(p) //nolint:gosec // test fixture under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rar3UnpackedSize(openRAR(t, b)); err != nil || got != 9 {
		t.Errorf("rar3UnpackedSize = %d, %v; want 9, nil", got, err)
	}
	mainEnd := len(rar3Sig) + 13
	if _, err := rar3UnpackedSize(openRAR(t, b[:mainEnd+rar3BaseHeader+10])); err == nil {
		t.Error("rar3UnpackedSize(truncated file header) err = nil")
	}
	bad := append([]byte(nil), b...)
	bad[mainEnd+5], bad[mainEnd+6] = 3, 0
	if _, err := rar3UnpackedSize(openRAR(t, bad)); err == nil {
		t.Error("rar3UnpackedSize(header size 3) err = nil")
	}
}
