package par2

import (
	"bytes"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// openBytes writes content to a temp file and opens it for reading.
func openBytes(t *testing.T, content []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.par2")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() }) //nolint:errcheck // read-only test file
	return f
}

func TestReadNextPacketWithOptions(t *testing.T) {
	t.Parallel()

	setID := [16]byte{1}
	creator := buildPacket(setID, typeCreator, []byte("gonzbd test\x00"))
	big := buildPacket(setID, typeCreator, bytes.Repeat([]byte{'x'}, int(MinPar2PacketBodySize)+4))
	badMD5 := append([]byte(nil), creator...)
	badMD5[len(badMD5)-2] ^= 0xff
	shortLen := append([]byte(nil), creator...)
	shortLen[8] = 10 // packet length below the 64-byte header
	for i := 9; i < 16; i++ {
		shortLen[i] = 0
	}

	cases := []struct {
		name     string
		content  []byte
		opts     ParseOptions
		wantBody int // body length of the packet returned; 0 means an error
		wantErr  error
	}{
		{name: "valid packet", content: creator, wantBody: len(creator) - 64},
		{name: "junk before the packet is skipped", content: append(bytes.Repeat([]byte{0xAA}, 200), creator...), wantBody: len(creator) - 64},
		{name: "a packet failing its MD5 is dropped", content: append(append([]byte(nil), badMD5...), creator...), wantBody: len(creator) - 64},
		{name: "body over the limit", content: big, opts: ParseOptions{MaxPacketBodySize: MinPar2PacketBodySize}, wantErr: ErrPacketBodySizeExceeded},
		{name: "a limit under the minimum means the default", content: big, opts: ParseOptions{MaxPacketBodySize: 10}, wantBody: len(big) - 64},
		{name: "length shorter than a header", content: shortLen},
		{name: "empty file", content: nil, wantErr: io.EOF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := openBytes(t, tc.content)
			header := make([]byte, 64)
			typ, body, err := readNextPacketWithOptions(f, uint64(len(tc.content)), header, tc.opts)
			if tc.wantBody == 0 {
				if err == nil {
					t.Fatalf("err = nil, want an error (body %d bytes)", len(body))
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if typ != typeCreator || len(body) != tc.wantBody {
				t.Errorf("packet = %x with %d-byte body, want the creator packet's %d bytes", typ, len(body), tc.wantBody)
			}
		})
	}
}

func TestScanForMagicWithOptions(t *testing.T) {
	t.Parallel()

	// The caller has read a 64-byte header that was not magic; the scan steps
	// back 63 bytes from there.
	withJunk := func(junk int) []byte {
		return append(bytes.Repeat([]byte{0x55}, junk), append(append([]byte(nil), magic...), 0, 0, 0)...)
	}
	cases := []struct {
		name      string
		content   []byte
		limit     int64
		wantFound bool
		wantPos   int64
		wantErr   error
	}{
		{name: "magic within the limit", content: withJunk(2000), limit: 4096, wantFound: true, wantPos: 2000},
		{name: "magic past the limit", content: withJunk(4000), limit: MinPar2JunkScanBytes, wantErr: ErrJunkScanLimitExceeded},
		{name: "a limit under the minimum means the default", content: withJunk(4000), limit: 10, wantFound: true, wantPos: 4000},
		{name: "no magic before end of file", content: bytes.Repeat([]byte{0x55}, 300), limit: 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := openBytes(t, tc.content)
			if _, err := io.ReadFull(f, make([]byte, 64)); err != nil {
				t.Fatal(err)
			}
			found, err := scanForMagicWithOptions(f, magic, tc.limit)
			if found != tc.wantFound {
				t.Fatalf("found = %v (err %v), want %v", found, err, tc.wantFound)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if !found {
				if tc.wantErr == nil && errors.Is(err, ErrJunkScanLimitExceeded) {
					t.Errorf("err = %v for a file that ended inside the limit", err)
				}
				return
			}
			if pos, _ := f.Seek(0, io.SeekCurrent); pos != tc.wantPos {
				t.Errorf("cursor at %d, want the magic at %d", pos, tc.wantPos)
			}
		})
	}
}

func TestPostProcessSet(t *testing.T) {
	t.Parallel()

	// 100 bytes over 64-byte slices: one full slice and a 36-byte tail, whose
	// IFSC CRC covers the tail zero-padded to 64 bytes.
	content := bytes.Repeat([]byte("0123456789"), 10)
	padded := append(append([]byte(nil), content[64:]...), make([]byte, 28)...)
	idC := [16]byte{'c'}
	set := &ParsedSet{
		SliceSize: 64,
		Files: []FileDesc{
			{FileID: [16]byte{'a'}, Hash16k: [16]byte{9}, FileSize: 10},
			{FileID: [16]byte{'b'}, Hash16k: [16]byte{9}, FileSize: 10},
			{FileID: idC, Hash16k: [16]byte{7}, FileSize: uint64(len(content))},
		},
		FilesByID: make(map[[16]byte]*FileDesc),
		By16k:     make(map[[16]byte]*FileDesc),
	}
	ifsc := []ifscData{{fileID: idC, slices: []ifscSlice{
		{crc32: crc32.ChecksumIEEE(content[:64])},
		{crc32: crc32.ChecksumIEEE(padded)},
	}}}

	postProcessSet(set, ifsc)

	if len(set.FilesByID) != 3 || set.FilesByID[idC] != &set.Files[2] {
		t.Errorf("FilesByID = %v, want all three entries, pointing into Files", set.FilesByID)
	}
	if got, want := set.Files[2].FileCRC32, crc32.ChecksumIEEE(content); got != want {
		t.Errorf("FileCRC32 = %08x, want %08x rebuilt from the slices", got, want)
	}
	if !set.Files[0].HasDuplicate || !set.Files[1].HasDuplicate || set.Files[2].HasDuplicate {
		t.Errorf("HasDuplicate = %v, %v, %v; want the two sharing a Hash16k marked",
			set.Files[0].HasDuplicate, set.Files[1].HasDuplicate, set.Files[2].HasDuplicate)
	}
	if len(set.By16k) != 1 || set.By16k[[16]byte{7}] != &set.Files[2] {
		t.Errorf("By16k = %v, want only the unshared Hash16k", set.By16k)
	}

	// With no slice size there is nothing to rebuild a CRC from.
	noSlices := &ParsedSet{
		Files:     []FileDesc{{FileID: idC, FileSize: uint64(len(content))}},
		FilesByID: make(map[[16]byte]*FileDesc),
		By16k:     make(map[[16]byte]*FileDesc),
	}
	postProcessSet(noSlices, ifsc)
	if noSlices.Files[0].FileCRC32 != 0 {
		t.Errorf("FileCRC32 = %08x with no slice size, want 0", noSlices.Files[0].FileCRC32)
	}
}
